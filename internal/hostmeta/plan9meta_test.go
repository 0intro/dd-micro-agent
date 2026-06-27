package hostmeta

import "testing"

func TestParsePlan9Cputype(t *testing.T) {
	tests := []struct {
		in        string
		wantModel string
		wantMhz   float64
	}{
		{"386 800\n", "386", 800},
		{"AMD64 2400\n", "AMD64", 2400},
		// Real X86type names contain spaces. MHz is the last field and the model
		// is everything before it (sys/src/9/pc/devarch.c X86type table).
		{"Core i7/Xeon 3400\n", "Core i7/Xeon", 3400},
		{"AMD Geode GX1 500\n", "AMD Geode GX1", 500},
		{"AMD-K10 Opteron G34 2200\n", "AMD-K10 Opteron G34", 2200},
		{"AMD64\n", "AMD64", 0},               // name only, no MHz
		{"AMD Geode LX\n", "AMD Geode LX", 0}, // multi-word name, no MHz
		{"", "", 0},
	}
	for _, tt := range tests {
		model, mhz := parsePlan9Cputype(tt.in)
		if model != tt.wantModel || mhz != tt.wantMhz {
			t.Errorf("parsePlan9Cputype(%q) = %q,%v; want %q,%v", tt.in, model, mhz, tt.wantModel, tt.wantMhz)
		}
	}
}

func TestParsePlan9MemTotal(t *testing.T) {
	data := "104857600 memory\n4096 pagesize\n15360/25600 user\n"
	if got := parsePlan9MemTotal(data); got != 104857600 {
		t.Errorf("parsePlan9MemTotal = %d, want 104857600", got)
	}
	if got := parsePlan9MemTotal("no memory line here\n"); got != 0 {
		t.Errorf("parsePlan9MemTotal(absent) = %d, want 0", got)
	}
}

func TestCountPlan9CPUs(t *testing.T) {
	data := "         0          0          0          0          0          0          0        250         90          5\n" +
		"         1          0          0          0          0          0          0        150         80          3\n"
	if got := countPlan9CPUs(data); got != 2 {
		t.Errorf("countPlan9CPUs = %d, want 2", got)
	}
}
