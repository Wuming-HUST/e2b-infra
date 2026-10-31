//go:build linux

package main

import "testing"

func TestBootLayoutRequiresExplicitPmemAndAbsoluteInit(t *testing.T) {
	good := "e2b.pmem-rootfs=1 e2b.pmem-gpa=0x4000000000 e2b.pmem-size=134217728 e2b.real-init=/sbin/init"
	if _, e := parseBoot(good); e != nil {
		t.Fatal(e)
	}
	if c, err := parseBoot(good + " e2b.pmem-data-offset=2097152"); err != nil || c.dataOffset != 2<<20 {
		t.Fatalf("PFN namespace: %#v %v", c, err)
	}
	for _, s := range []string{"", "e2b.pmem-rootfs=1", good + " e2b.pmem-size=0", good + " e2b.pmem-gpa=0xffffffffffffffff", good + " e2b.real-init=../init", good + " e2b.pmem-size=4096", good + " e2b.pmem-gpa=NaN", good + " e2b.pmem-data-offset=4096", good + " e2b.pmem-data-offset=18446744073709551615"} {
		if _, e := parseBoot(s); e == nil {
			t.Fatalf("accepted %q", s)
		}
	}
}
