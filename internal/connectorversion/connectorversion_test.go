package connectorversion

import "testing"

func TestParse(t *testing.T) {
	for in, want := range map[string]Client{
		"wakeshift/2026.10.20.1435+91faaf7 codex": {Binary, "2026.10.20.1435+91faaf7", "codex"},
		"wakeshift-hermes/dev hermes":             {Hermes, "dev", "hermes"},
		"wakeshift-openclaw/0.1.0":                {OpenClaw, "0.1.0", ""},
	} {
		got, ok := Parse(in)
		if !ok || got != want {
			t.Errorf("Parse(%q) = %+v, %v; want %+v", in, got, ok, want)
		}
		if got.String() != in {
			t.Errorf("%+v.String() = %q, want %q", got, got.String(), in)
		}
	}
	for _, bad := range []string{"", "wakeshift", "wakeshift/", "Wakeshift/1 codex", "wakeshift/1 codex extra", "wakeshift/1 CODEX", "wakeshift/1;rm codex", "/1 codex"} {
		if c, ok := Parse(bad); ok {
			t.Errorf("Parse(%q) = %+v, want no client", bad, c)
		}
	}
}

func TestIdentify(t *testing.T) {
	if c := Identify("", "hermes"); c != (Client{Hermes, "0", "hermes"}) {
		t.Errorf("no header = %+v, want version 0 of the harness's connector", c)
	}
	if c := Identify("", "codex"); c != (Client{Binary, "0", "codex"}) {
		t.Errorf("no header = %+v", c)
	}
	if c := Identify("garbage", "openclaw"); c != (Client{OpenClaw, "0", "openclaw"}) {
		t.Errorf("a bad header = %+v, want it read as none", c)
	}
	if c := Identify("wakeshift/2026.10.20", "cursor"); c != (Client{Binary, "2026.10.20", "cursor"}) {
		t.Errorf("a header without a harness = %+v, want the conversation's", c)
	}
	if c := Identify("", "Not A Harness"); c != (Client{Binary, "0", ""}) {
		t.Errorf("an odd harness = %+v", c)
	}
	// A name that isn't the harness's connector escapes no minimum.
	for _, h := range []string{"made-up/2099.1.1 codex", "wakeshift-hermes/2099.1.1 codex", "wakeshift/2099.1.1 hermes"} {
		if c := Identify(h, "codex"); c.Version != "0" {
			t.Errorf("Identify(%q) = %+v, want version 0", h, c)
		}
	}
}

func TestCompare(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"2026.10.20.1435+abc", "2026.10.20", 1},
		{"2026.10.20", "2026.10.20.0", 0},
		{"2026.10.19.2359+abc", "2026.10.20", -1},
		{"2026.9.30", "2026.10.1", -1},
		{"dev", "2026.10.20", -1},
		{"0", "0.0.1", -1},
		{"abc1234", "0", 0},
		{"1.2.3-rc1", "1.2.3", 0},
		{"10.0", "9.99", 1},
	} {
		if got := Compare(c.a, c.b); got != c.want {
			t.Errorf("Compare(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestMinimums(t *testing.T) {
	m, err := ParseMinimums(" wakeshift=2026.10.20, wakeshift:codex=2026.10.21.1200 ,wakeshift-hermes=2026.10.20")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		c    Client
		want bool
	}{
		{Client{Binary, "2026.10.20.0001+x", "cursor"}, false},
		{Client{Binary, "2026.10.19.2359+x", "cursor"}, true},
		{Client{Binary, "2026.10.20.0001+x", "codex"}, true}, // codex's own minimum wins
		{Client{Binary, "2026.10.21.1200+x", "codex"}, false},
		{Client{Binary, "0", ""}, true},
		{Client{Hermes, "dev", "hermes"}, true},
		{Client{OpenClaw, "0", "openclaw"}, false}, // no minimum for it
	} {
		if got := m.Outdated(c.c); got != c.want {
			t.Errorf("Outdated(%v) = %v, want %v", c.c, got, c.want)
		}
	}
	none, err := ParseMinimums("")
	if err != nil || none.Outdated(Client{Binary, "0", "codex"}) {
		t.Errorf("no minimums refused version 0 (%v)", err)
	}
	for _, bad := range []string{"wakeshift", "wakeshift=", "wakeshift=dev", "Wake=1", "wakeshift:=1", "wakeshift:Codex=1"} {
		if _, err := ParseMinimums(bad); err == nil {
			t.Errorf("ParseMinimums(%q) took it", bad)
		}
	}
}

// Production's minimum (deploy/terraform/production/terraform.tfvars):
// the binary from v1.4.0's own build, the plugins from v1.1.0's; every
// build before its minimum (a minute before, earlier releases) is refused.
func TestReleaseMinimum(t *testing.T) {
	m, err := ParseMinimums("wakeshift=2026.10.30.0132,wakeshift-openclaw=2026.10.26.2256,wakeshift-hermes=2026.10.26.2256")
	if err != nil {
		t.Fatal(err)
	}
	all := []string{Binary, OpenClaw, Hermes}
	plugins := []string{OpenClaw, Hermes}
	for _, c := range []struct {
		names   []string
		version string
		want    bool
	}{
		{all, "2026.10.30.0132+be7032b", false}, // v1.4.0
		{all, "2026.10.30.0142+b0190dc", false}, // later
		{[]string{Binary}, "2026.10.30.0131+be7032b", true},
		{[]string{Binary}, "2026.10.30.0122+be7032b", true}, // staging's build of v1.4.0's commit
		{[]string{Binary}, "2026.10.29.2127+145cb04", true}, // v1.3.1
		{[]string{Binary}, "2026.10.29.1646+8b2f274", true}, // v1.3.0
		{[]string{Binary}, "2026.10.26.2256+9534799", true}, // v1.1.0
		{plugins, "2026.10.26.2256+9534799", false},         // v1.1.0
		{plugins, "2026.10.26.2255+9534799", true},
		{plugins, "2026.10.26.0532+bde2a26", true}, // v1.0.0
		{all, "0", true},
		{all, "dev", true},
	} {
		for _, name := range c.names {
			cl := Client{Name: name, Version: c.version, Harness: "codex"}
			if got := m.Outdated(cl); got != c.want {
				t.Errorf("Outdated(%v) = %v, want %v", cl, got, c.want)
			}
		}
	}
}

func TestValue(t *testing.T) {
	if v := Value(Binary, "2026.10.20.1435+91faaf7", "codex"); v != "wakeshift/2026.10.20.1435+91faaf7 codex" {
		t.Errorf("Value = %q", v)
	}
	if v := Value(Binary, "", "codex"); v != "wakeshift/0 codex" {
		t.Errorf("Value without a version = %q", v)
	}
}
