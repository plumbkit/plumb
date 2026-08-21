package mcp

// Lookup returns the registered Tool for name and whether it was found. Safe
// for concurrent use. Independent of ToolFilter, like ToolNames — it reaches
// any registered tool, not just the advertised set. Lets a caller probe the
// REAL object a registration path (e.g. registerAllTools) built and wired,
// rather than reconstructing a parallel instance that could drift from it.
func (s *Server) Lookup(name string) (Tool, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.tools[name]
	return t, ok
}
