package payment

// SetReconcileBatch lowers the per-run cap of ReconcilePayments (tests).
func SetReconcileBatch(s *Service, n int) { s.reconcileBatch = n }
