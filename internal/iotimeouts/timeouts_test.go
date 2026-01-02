package iotimeouts

import (
	"testing"
)

func TestTimeoutCoordination(t *testing.T) {
	// Verify the host timeout is greater than the guest timeout.
	// This is critical for the timeout hierarchy to work correctly.
	if HostIOWaitTimeout <= SubscriberWaitTimeout {
		t.Errorf("HostIOWaitTimeout (%v) must be > SubscriberWaitTimeout (%v)",
			HostIOWaitTimeout, SubscriberWaitTimeout)
	}

	// The host timeout should account for:
	// - Guest subscriber timeout
	// - Network latency (~1s)
	// - FIFO flush time (~1s)
	// So host should be at least 12s greater than guest timeout for safety.
	minMargin := SubscriberWaitTimeout + 12e9 // 12 seconds in nanoseconds
	if HostIOWaitTimeout < minMargin {
		t.Errorf("HostIOWaitTimeout (%v) should be at least %v to account for network latency and FIFO flush",
			HostIOWaitTimeout, minMargin)
	}
}

func TestRetryConstants(t *testing.T) {
	// Verify retry delay bounds make sense.
	if OutputRetryInitialDelay >= OutputRetryMaxDelay {
		t.Errorf("OutputRetryInitialDelay (%v) should be < OutputRetryMaxDelay (%v)",
			OutputRetryInitialDelay, OutputRetryMaxDelay)
	}
}

func TestBufferConstants(t *testing.T) {
	// Verify buffer sizes are reasonable.
	if SubscriberChannelBuffer <= 0 {
		t.Errorf("SubscriberChannelBuffer must be positive, got %d", SubscriberChannelBuffer)
	}

	if MaxBufferedBytes <= 0 {
		t.Errorf("MaxBufferedBytes must be positive, got %d", MaxBufferedBytes)
	}

	// The channel buffer times max chunk size should not exceed MaxBufferedBytes
	// too much, or we risk excessive memory usage per subscriber.
	maxChunkSize := 32 * 1024 // 32KB, typical chunk size
	theoreticalMax := SubscriberChannelBuffer * maxChunkSize
	// Allow up to 10x MaxBufferedBytes for burst handling
	if theoreticalMax > MaxBufferedBytes*10 {
		t.Logf("Warning: SubscriberChannelBuffer * 32KB = %d, which is >10x MaxBufferedBytes (%d)",
			theoreticalMax, MaxBufferedBytes)
	}
}
