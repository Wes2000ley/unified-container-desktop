package engine

// activeOperations includes detached streams whose opening RPC has already
// returned. Resource Saver must wait for their producers and transfers to end.
func (b *Backend) activeOperations() int {
	active := 0
	b.sessionMu.Lock()
	for _, session := range b.sessions {
		select {
		case <-session.finished:
		default:
			active++
		}
	}
	b.sessionMu.Unlock()
	b.transferMu.Lock()
	for _, transfer := range b.transfers {
		select {
		case <-transfer.done:
		default:
			active++
		}
	}
	b.transferMu.Unlock()
	return active
}
