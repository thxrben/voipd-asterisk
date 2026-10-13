package sysinfo

import "testing"

func TestReadOnLinux(t *testing.T) {
	in := Read(t.TempDir())
	if in.CPUs == 0 || in.Load1 == nil || in.MemTotal == nil || in.DiskFree == nil || in.UptimeSecs == nil {
		t.Errorf("missing values: %+v", in)
	}
	if in.MemAvail != nil && *in.MemAvail > *in.MemTotal {
		t.Errorf("available > total")
	}
}
