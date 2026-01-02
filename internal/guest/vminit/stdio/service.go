package stdio

import (
	"context"
	"errors"
	"io"

	"github.com/containerd/errdefs"
	"github.com/containerd/errdefs/pkg/errgrpc"
	"github.com/containerd/log"
	"github.com/containerd/ttrpc"

	stdiov1 "github.com/aledbf/qemubox/containerd/api/services/stdio/v1"
)

// service implements the StdIOService TTRPC interface.
type service struct {
	manager *Manager
}

// NewService creates a new StdIO service backed by the given manager.
func NewService(manager *Manager) *service {
	return &service{manager: manager}
}

// RegisterTTRPC registers the service with a TTRPC server.
func (s *service) RegisterTTRPC(server *ttrpc.Server) error {
	stdiov1.RegisterStdIOService(server, s)
	return nil
}

// WriteStdin writes data to a process's stdin.
func (s *service) WriteStdin(ctx context.Context, req *stdiov1.WriteStdinRequest) (*stdiov1.WriteStdinResponse, error) {
	log.G(ctx).WithField("container", req.ContainerId).WithField("exec", req.ExecId).WithField("len", len(req.Data)).Debug("WriteStdin")

	n, err := s.manager.WriteStdin(req.ContainerId, req.ExecId, req.Data)
	if err != nil {
		return nil, toGRPCError(err)
	}

	return &stdiov1.WriteStdinResponse{
		BytesWritten: uint32(n),
	}, nil
}

// ReadStdout streams stdout data from a process.
func (s *service) ReadStdout(ctx context.Context, req *stdiov1.ReadOutputRequest, stream stdiov1.StdIO_ReadStdoutServer) error {
	log.G(ctx).WithField("container", req.ContainerId).WithField("exec", req.ExecId).Debug("ReadStdout started")

	ch, err := s.manager.SubscribeStdout(ctx, req.ContainerId, req.ExecId)
	if err != nil {
		return toGRPCError(err)
	}

	return s.streamOutput(ctx, ch, stream, "stdout", req.ContainerId, req.ExecId)
}

// ReadStderr streams stderr data from a process.
func (s *service) ReadStderr(ctx context.Context, req *stdiov1.ReadOutputRequest, stream stdiov1.StdIO_ReadStderrServer) error {
	log.G(ctx).WithField("container", req.ContainerId).WithField("exec", req.ExecId).Debug("ReadStderr started")

	ch, err := s.manager.SubscribeStderr(ctx, req.ContainerId, req.ExecId)
	if err != nil {
		return toGRPCError(err)
	}

	return s.streamOutput(ctx, ch, stream, "stderr", req.ContainerId, req.ExecId)
}

// outputSender abstracts the Send method for both stdout and stderr streams.
type outputSender interface {
	Send(*stdiov1.OutputChunk) error
}

// streamOutput streams data from the channel to the RPC stream.
// It uses a "biased select" pattern (from Kata Containers) that prioritizes
// draining data from the channel before honoring context cancellation.
// This ensures no data is lost when the context is cancelled while data
// is still buffered in the channel.
func (s *service) streamOutput(ctx context.Context, ch <-chan OutputData, stream outputSender, streamName, containerID, execID string) error {
	for {
		// Biased select: always try to drain data first (non-blocking).
		// This prevents data loss when ctx.Done() fires while data is available.
		select {
		case data, ok := <-ch:
			if !ok {
				// Channel closed, send EOF.
				log.G(ctx).WithField("container", containerID).WithField("stream", streamName).Debug("stream channel closed")
				return stream.Send(&stdiov1.OutputChunk{Eof: true})
			}

			if err := s.sendChunk(ctx, stream, data, streamName, containerID); err != nil {
				return err
			}
			if data.EOF {
				return nil
			}
			continue // Loop back to drain more data
		default:
			// No data available, proceed to blocking select
		}

		// No data immediately available - now wait for either data or cancellation.
		select {
		case <-ctx.Done():
			// Context cancelled, but drain any remaining data first.
			log.G(ctx).WithField("container", containerID).WithField("stream", streamName).Debug("stream context cancelled, draining remaining data")
			return s.drainAndClose(ctx, ch, stream, streamName, containerID)

		case data, ok := <-ch:
			if !ok {
				// Channel closed, send EOF.
				log.G(ctx).WithField("container", containerID).WithField("stream", streamName).Debug("stream channel closed")
				return stream.Send(&stdiov1.OutputChunk{Eof: true})
			}

			if err := s.sendChunk(ctx, stream, data, streamName, containerID); err != nil {
				return err
			}
			if data.EOF {
				return nil
			}
		}
	}
}

// sendChunk sends a single chunk to the stream.
func (s *service) sendChunk(ctx context.Context, stream outputSender, data OutputData, streamName, containerID string) error {
	log.G(ctx).WithField("container", containerID).WithField("stream", streamName).
		WithField("bytes", len(data.Data)).WithField("eof", data.EOF).Debug("received chunk from channel")

	chunk := &stdiov1.OutputChunk{
		Data: data.Data,
		Eof:  data.EOF,
	}

	if err := stream.Send(chunk); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		log.G(ctx).WithError(err).WithField("container", containerID).WithField("stream", streamName).Warn("error sending chunk")
		return err
	}

	if data.EOF {
		log.G(ctx).WithField("container", containerID).WithField("stream", streamName).Debug("stream EOF")
	}
	return nil
}

// drainAndClose drains remaining data from the channel after context cancellation.
// This ensures no data is lost even when the RPC context is cancelled.
func (s *service) drainAndClose(ctx context.Context, ch <-chan OutputData, stream outputSender, streamName, containerID string) error {
	for {
		select {
		case data, ok := <-ch:
			if !ok {
				// Channel closed
				log.G(ctx).WithField("container", containerID).WithField("stream", streamName).Debug("channel closed while draining")
				return stream.Send(&stdiov1.OutputChunk{Eof: true})
			}

			// Best effort send - ignore errors since context is already cancelled
			if err := s.sendChunk(ctx, stream, data, streamName, containerID); err != nil {
				log.G(ctx).WithError(err).WithField("container", containerID).WithField("stream", streamName).Debug("error sending during drain")
				return ctx.Err()
			}
			if data.EOF {
				return nil
			}
		default:
			// No more data to drain
			log.G(ctx).WithField("container", containerID).WithField("stream", streamName).Debug("drain complete, no more data")
			return ctx.Err()
		}
	}
}

// CloseStdin closes a process's stdin.
func (s *service) CloseStdin(ctx context.Context, req *stdiov1.CloseStdinRequest) (*stdiov1.CloseStdinResponse, error) {
	log.G(ctx).WithField("container", req.ContainerId).WithField("exec", req.ExecId).Debug("CloseStdin")

	if err := s.manager.CloseStdin(req.ContainerId, req.ExecId); err != nil {
		return nil, toGRPCError(err)
	}

	return &stdiov1.CloseStdinResponse{}, nil
}

// toGRPCError converts an error to a GRPC-compatible error.
func toGRPCError(err error) error {
	if errdefs.IsNotFound(err) {
		return errgrpc.ToGRPC(err)
	}
	if errdefs.IsFailedPrecondition(err) {
		return errgrpc.ToGRPC(err)
	}
	return err
}
