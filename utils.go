package uringnet

import "unsafe"

// BytesToString converts byte slice to a string without memory allocation.
// The bytes must not be modified while the string is in use.
func BytesToString(b []byte) string {
	return unsafe.String(unsafe.SliceData(b), len(b))
}

// StringToBytes converts string to a byte slice without memory allocation.
// The returned slice must not be modified.
func StringToBytes(s string) []byte {
	return unsafe.Slice(unsafe.StringData(s), len(s))
}
