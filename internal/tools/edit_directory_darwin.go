package tools

// The macOS SDK defines O_SEARCH as O_EXEC | O_DIRECTORY. x/sys/unix does
// not expose O_SEARCH, so use the SDK's O_EXEC value with the common flags.
const editDirectorySearchFlag = 0x40000000

func unsupportedEditRootStat(_ error) bool { return false }
