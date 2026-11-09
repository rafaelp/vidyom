package ffmpeg

// ExportedMinimalEnvForTest exposes the child environment to the external test package.
func ExportedMinimalEnvForTest() []string { return minimalEnv() }
