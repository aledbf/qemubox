package events

import (
	"context"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/pkg/namespaces"
	"google.golang.org/protobuf/types/known/emptypb"
)

func TestExchange(t *testing.T) {
	t.Run("NewExchange returns non-nil", func(t *testing.T) {
		ex := NewExchange()
		if ex == nil {
			t.Fatal("NewExchange() returned nil")
		}
	})

	t.Run("publish and subscribe", func(t *testing.T) {
		ex := NewExchange()
		ctx := namespaces.WithNamespace(context.Background(), "default")

		evCh, errCh := ex.Subscribe(ctx)

		if err := ex.Publish(ctx, "/test/subscribe", &emptypb.Empty{}); err != nil {
			t.Fatalf("Publish() failed: %v", err)
		}

		select {
		case env := <-evCh:
			if env == nil {
				t.Fatal("received nil envelope")
			}
			if env.Topic != "/test/subscribe" {
				t.Fatalf("topic = %q, want %q", env.Topic, "/test/subscribe")
			}
		case err := <-errCh:
			t.Fatalf("unexpected error: %v", err)
		case <-time.After(time.Second):
			t.Fatal("timeout waiting for event")
		}
	})

	t.Run("publish requires namespace", func(t *testing.T) {
		ex := NewExchange()
		ctx := context.Background()

		if err := ex.Publish(ctx, "/test/topic", &emptypb.Empty{}); err == nil {
			t.Fatal("expected error for missing namespace, got nil")
		}
	})
}
