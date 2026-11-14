package usage

import "io/fs"

// inodesSupported: os.Stat reports no file index on Windows; a replaced
// transcript is told apart only when it shrank.
const inodesSupported = false

// fileID is empty on Windows (see inodesSupported).
func fileID(fs.FileInfo) string { return "" }
