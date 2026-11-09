package local

// CloseLogForTest closes the access log, leaving the instance usable, to prove that
// a failing log fails the operation.
func (s *Storage) CloseLogForTest() error { return s.log.close() }
