package booking

import "github.com/google/uuid"

// SetJobHooks overrides the job batch size (0 keeps the default) and sets the
// per-item hook run inside each item's transaction (nil clears it).
func (s *Service) SetJobHooks(batch int32, before func(id uuid.UUID) error) {
	s.batchSize = batch
	s.beforeItem = before
}
