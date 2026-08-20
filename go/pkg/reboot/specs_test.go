package reboot

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseSpecBareName(t *testing.T) {
	host, err := ParseSpec("web1")
	if err != nil {
		t.Fatal(err)
	}
	if host.Name != "web1" {
		t.Errorf("Name = %q, want web1", host.Name)
	}
	// The address is only defaulted at the point of use, so that a later
	// definition supplying a real one is not mistaken for an override.
	if host.Addr != "" {
		t.Errorf("Addr = %q, want empty", host.Addr)
	}
	if host.Target() != "web1" {
		t.Errorf("Target() = %q, want web1", host.Target())
	}
}

func TestParseSpecAllFields(t *testing.T) {
	host, err := ParseSpec(
		"vm-a,addr=10.0.0.21,user=admin,ssh-arg=-4,ssh-arg=-C,after=hv1,after=dns1")
	if err != nil {
		t.Fatal(err)
	}
	want := Host{
		Name:    "vm-a",
		Addr:    "10.0.0.21",
		User:    "admin",
		SSHArgs: []string{"-4", "-C"},
		After:   []string{"hv1", "dns1"},
	}
	if !reflect.DeepEqual(host, want) {
		t.Errorf("ParseSpec() = %+v, want %+v", host, want)
	}
	if host.Target() != "10.0.0.21" {
		t.Errorf("Target() = %q, want 10.0.0.21", host.Target())
	}
}

func TestParseSpecErrors(t *testing.T) {
	tests := []struct {
		name string
		spec string
		want string
	}{
		{"empty", "", "no host name"},
		{"leading field", "addr=10.0.0.1,user=root", "host name must come first"},
		{"not key=value", "web1,justtext", "is not key=value"},
		{"empty value", "web1,addr=", "empty value"},
		{"unknown field", "web1,colour=blue", `unknown field "colour"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseSpec(tt.spec)
			if err == nil {
				t.Fatalf("ParseSpec(%q) succeeded, want error", tt.spec)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestParseSpecQuoting(t *testing.T) {
	tests := []struct {
		name string
		spec string
		want Host
	}{
		{
			// Unquoted, the value would be truncated at the comma and its
			// remainder read as another key.
			name: "quoted field keeps its comma",
			spec: `web1,"ssh-arg=-oCiphers=aes128-ctr,aes256-ctr"`,
			want: Host{Name: "web1", SSHArgs: []string{"-oCiphers=aes128-ctr,aes256-ctr"}},
		},
		{
			// Backslashes carry no meaning in this format, so a Windows-style
			// user or an ssh argument containing one passes through untouched.
			name: "backslashes are literal",
			spec: `web1,user=domain\admin`,
			want: Host{Name: "web1", User: `domain\admin`},
		},
		{
			name: "spaces survive inside a field",
			spec: `web1,ssh-arg=-oProxyCommand=nc %h %p`,
			want: Host{Name: "web1", SSHArgs: []string{"-oProxyCommand=nc %h %p"}},
		},
		{
			// Whitespace around fields is stripped: a hand-written host file
			// should not produce a host name that matches nothing.
			name: "surrounding whitespace is trimmed",
			spec: ` web1 , addr=10.0.0.4 `,
			want: Host{Name: "web1", Addr: "10.0.0.4"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			host, err := ParseSpec(tt.spec)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(host, tt.want) {
				t.Errorf("ParseSpec(%q) = %+v, want %+v", tt.spec, host, tt.want)
			}
		})
	}
}

func TestFormatSpecRoundTrip(t *testing.T) {
	original := Host{
		Name:    "vm-a",
		Addr:    "10.0.0.21",
		User:    "admin",
		SSHArgs: []string{"-o", "Ciphers=aes128-ctr,aes256-ctr", `back\slash`, `quo"te`},
		After:   []string{"hv1", "dns1"},
	}
	spec := FormatSpec(original)
	parsed, err := ParseSpec(spec)
	if err != nil {
		t.Fatalf("ParseSpec(%q): %v", spec, err)
	}
	if !reflect.DeepEqual(parsed, original) {
		t.Errorf("round trip through %q gave %+v, want %+v", spec, parsed, original)
	}
}

func TestFormatSpecOmitsEmptyFields(t *testing.T) {
	tests := []struct {
		name string
		host Host
		want string
	}{
		{"name only", Host{Name: "web1"}, "web1"},
		{"name and addr", Host{Name: "web1", Addr: "10.0.0.1"}, "web1,addr=10.0.0.1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := FormatSpec(tt.host); got != tt.want {
				t.Errorf("FormatSpec() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestReadSpecs(t *testing.T) {
	input := strings.Join([]string{
		"# generated from inventory.yml",
		"",
		"hv1,addr=10.0.0.5",
		"  vm-a,addr=10.0.0.21,after=hv1  ",
		"",
	}, "\n")

	hosts, err := ReadSpecs(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	want := []Host{
		{Name: "hv1", Addr: "10.0.0.5"},
		{Name: "vm-a", Addr: "10.0.0.21", After: []string{"hv1"}},
	}
	if !reflect.DeepEqual(hosts, want) {
		t.Errorf("ReadSpecs() = %+v, want %+v", hosts, want)
	}
}

func TestReadSpecsReportsBadLine(t *testing.T) {
	_, err := ReadSpecs(strings.NewReader("hv1\nvm-a,nonsense\n"))
	if err == nil {
		t.Fatal("ReadSpecs() succeeded, want error")
	}
	if !strings.Contains(err.Error(), "is not key=value") {
		t.Errorf("error = %q, want it to explain the bad field", err)
	}
}

func TestBuildPlanTargetsOnly(t *testing.T) {
	targets, err := ParseSpecs([]string{"hv1,addr=10.0.0.5", "vm-a,after=hv1"})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := BuildPlan(nil, targets, Defaults{}, false)
	if err != nil {
		t.Fatal(err)
	}
	equalStrings(t, "Targets", plan.Targets, []string{"hv1", "vm-a"})
	if plan.Hosts["hv1"].Addr != "10.0.0.5" {
		t.Errorf("hv1 addr = %q, want 10.0.0.5", plan.Hosts["hv1"].Addr)
	}
}

func TestBuildPlanContextIsNotTargeted(t *testing.T) {
	context, err := ParseSpecs([]string{"hv1,addr=10.0.0.5", "vm-a,addr=10.0.0.21,after=hv1"})
	if err != nil {
		t.Fatal(err)
	}
	targets, err := ParseSpecs([]string{"vm-a"})
	if err != nil {
		t.Fatal(err)
	}

	plan, err := BuildPlan(context, targets, Defaults{}, false)
	if err != nil {
		t.Fatal(err)
	}

	// Naming a host that stdin already defined targets that definition rather
	// than replacing it — otherwise every target would have to repeat its
	// address on the command line.
	equalStrings(t, "Targets", plan.Targets, []string{"vm-a"})
	if got := plan.Hosts["vm-a"].Addr; got != "10.0.0.21" {
		t.Errorf("vm-a addr = %q, want the piped-in 10.0.0.21", got)
	}
	if _, ok := plan.Hosts["hv1"]; !ok {
		t.Error("hv1 should remain known as context even though it is not targeted")
	}
}

func TestBuildPlanTargetOverridesContextField(t *testing.T) {
	context, _ := ParseSpecs([]string{"vm-a,addr=10.0.0.21,user=admin"})
	targets, _ := ParseSpecs([]string{"vm-a,user=root"})

	plan, err := BuildPlan(context, targets, Defaults{}, false)
	if err != nil {
		t.Fatal(err)
	}
	host := plan.Hosts["vm-a"]
	if host.User != "root" {
		t.Errorf("user = %q, want the command line override root", host.User)
	}
	if host.Addr != "10.0.0.21" {
		t.Errorf("addr = %q, want the inherited 10.0.0.21", host.Addr)
	}
}

func TestBuildPlanAllTargetsEveryContextHost(t *testing.T) {
	context, _ := ParseSpecs([]string{"hv1", "vm-a,after=hv1", "vm-b,after=hv1"})

	plan, err := BuildPlan(context, nil, Defaults{}, true)
	if err != nil {
		t.Fatal(err)
	}
	equalStrings(t, "Targets", plan.Targets, []string{"hv1", "vm-a", "vm-b"})
}

func TestBuildPlanDefaults(t *testing.T) {
	context, _ := ParseSpecs([]string{"hv1", "vm-a,user=root,ssh-arg=-4"})

	defaults := Defaults{User: "ops", SSHArgs: []string{"-o", "LogLevel=ERROR"}}
	plan, err := BuildPlan(context, nil, defaults, true)
	if err != nil {
		t.Fatal(err)
	}

	// The fleet-wide user fills in only where a host stated nothing itself.
	if got := plan.Hosts["hv1"].User; got != "ops" {
		t.Errorf("hv1 user = %q, want the default ops", got)
	}
	if got := plan.Hosts["vm-a"].User; got != "root" {
		t.Errorf("vm-a user = %q, want its own root", got)
	}
	// Fleet-wide ssh arguments extend a host's own rather than replacing them.
	equalStrings(t, "vm-a ssh args", plan.Hosts["vm-a"].SSHArgs,
		[]string{"-o", "LogLevel=ERROR", "-4"})
	equalStrings(t, "hv1 ssh args", plan.Hosts["hv1"].SSHArgs,
		[]string{"-o", "LogLevel=ERROR"})
}

func TestBuildPlanErrors(t *testing.T) {
	tests := []struct {
		name      string
		context   []string
		targets   []string
		targetAll bool
		want      string
	}{
		{
			name: "no hosts at all",
			want: "no hosts given",
		},
		{
			name:    "context with nothing targeted",
			context: []string{"hv1"},
			want:    "no hosts targeted",
		},
		{
			name:    "dependency outside the host set",
			targets: []string{"vm-a,after=ghost"},
			want:    `"ghost", which is not a known host`,
		},
		{
			// Caught before the confirmation prompt rather than after the first
			// tier has already rebooted.
			name:    "cycle rejected up front",
			targets: []string{"a,after=b", "b,after=a"},
			want:    "cyclic ordering",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			context, err := ParseSpecs(tt.context)
			if err != nil {
				t.Fatal(err)
			}
			targets, err := ParseSpecs(tt.targets)
			if err != nil {
				t.Fatal(err)
			}
			_, err = BuildPlan(context, targets, Defaults{}, tt.targetAll)
			if err == nil {
				t.Fatal("BuildPlan() succeeded, want error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestBuildPlanDeduplicatesRepeatedTarget(t *testing.T) {
	targets, _ := ParseSpecs([]string{"hv1", "hv1,user=root"})
	plan, err := BuildPlan(nil, targets, Defaults{}, false)
	if err != nil {
		t.Fatal(err)
	}
	equalStrings(t, "Targets", plan.Targets, []string{"hv1"})
	if got := plan.Hosts["hv1"].User; got != "root" {
		t.Errorf("user = %q, want root from the later mention", got)
	}
}
