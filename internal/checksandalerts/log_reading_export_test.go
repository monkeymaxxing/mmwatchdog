package checksandalerts

// ReadLogLinesForTest exposes file reading to external tests, including errors
// that cannot reliably be triggered between two reads of a regular file.
var ReadLogLinesForTest = readLogLines
