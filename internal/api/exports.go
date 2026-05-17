package api

// Deps returns the dependency bag the server was constructed with so a
// caller (the CLI wiring) can mutate fields like ShutdownFn after Bind.
func (s *Server) Deps() *Deps { return &s.deps }
