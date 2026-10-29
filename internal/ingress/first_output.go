package ingress

import "ferry/internal/domain"

// meaningfulSSEOutput inspects the actual client wire frame after conversion
// and buffering. Delivery and upstream waiting use the same output semantics.
func meaningfulSSEOutput(event string, payload []byte) bool {
	return domain.MeaningfulSSEOutput(event, payload)
}
