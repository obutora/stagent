package holder

// moveStderrToLog does nothing on Windows: writing to a pipe whose reader
// is gone fails there without ending the process.
func (h *Holder) moveStderrToLog() {}
