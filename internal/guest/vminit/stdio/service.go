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
	ch, done, err := s.manager.SubscribeStdout(ctx, req.ContainerId, req.ExecId)
	if err != nil {
		return toGRPCError(err)
	}
	defer done()

	return s.streamOutput(ctx, ch, stream, req.ContainerId)
}

// ReadStderr streams stderr data from a process.
func (s *service) ReadStderr(ctx context.Context, req *stdiov1.ReadOutputRequest, stream stdiov1.StdIO_ReadStderrServer) error {
	ch, done, err := s.manager.SubscribeStderr(ctx, req.ContainerId, req.ExecId)
	if err != nil {
		return toGRPCError(err)
	}
	defer done()

	return s.streamOutput(ctx, ch, stream, req.ContainerId)
}

// outputSender abstracts the Send method for both stdout and stderr streams.
type outputSender interface {
	Send(*stdiov1.OutputChunk) error
}

// streamOutput streams data from the channel to the RPC stream.
// It uses a "biased select" pattern that prioritizes draining data from the
// channel before honoring context cancellation to prevent data loss.
func (s *service) streamOutput(ctx context.Context, ch <-chan OutputData, stream outputSender, containerID string) error {
	for {
		// Biased select: always try to drain data first (non-blocking).
		select {
		case data, ok := <-ch:
			done, err := s.handleData(stream, data, ok)
			if err != nil || done {
				return err
			}
			continue
		default:
		}

		// No data immediately available - wait for data or cancellation.
		select {
		case <-ctx.Done():
			return s.drainRemaining(ctx, ch, stream, containerID)
		case data, ok := <-ch:
			done, err := s.handleData(stream, data, ok)
			if err != nil || done {
				return err
			}
		}
	}
}

// handleData processes a single data item from the channel.
// Returns (done, error) where done=true means streaming should stop.
func (s *service) handleData(stream outputSender, data OutputData, ok bool) (bool, error) {
	if !ok {
		// Channel closed, send EOF.
		return true, stream.Send(&stdiov1.OutputChunk{Eof: true})
	}

	chunk := &stdiov1.OutputChunk{
		Data: data.Data,
		Eof:  data.EOF,
	}
	if err := stream.Send(chunk); err != nil {
		if errors.Is(err, io.EOF) {
			return true, nil
		}
		return true, err
	}

	return data.EOF, nil
}

// drainRemaining drains any remaining data from the channel after context cancellation.
func (s *service) drainRemaining(ctx context.Context, ch <-chan OutputData, stream outputSender, containerID string) error {
	for {
		select {
		case data, ok := <-ch:
			done, err := s.handleData(stream, data, ok)
			if err != nil {
				// Best effort - log and return context error
				log.G(ctx).WithError(err).WithField("container", containerID).Debug("error sending during drain")
				return ctx.Err()
			}
			if done {
				return nil
			}
		default:
			// No more data to drain, return nil (not ctx.Err()) since we drained successfully
			return nil
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
