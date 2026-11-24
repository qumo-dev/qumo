package main

import "testing"

// BenchmarkHashAllFlat targets the SHA-256 + hex-encoding path that formats the
// smoketest connectivity-check digest (the fmt.Sprintf("%x") -> hex.EncodeToString
// change).
func BenchmarkHashAllFlat(b *testing.B) {
	data := generateTestData(10, 10, 1024)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = hashAllFlat(data, 10, 10)
	}
}
