package pluginhost

import "testing"

type sample struct{ kind Kind }

func (s sample) Name() string { return "sample" }
func (s sample) Kind() Kind   { return s.kind }

func TestDisabledHostRejectsPlugins(t *testing.T) {
	host := New(false)
	if err := host.Register(sample{kind: KindExecutor}); err == nil {
		t.Fatal("disabled host registered a plugin")
	}
}

func TestHostRejectsClassifierReplacement(t *testing.T) {
	host := New(true)
	if err := host.Register(sample{kind: KindClassifier}); err == nil || err.Error() != "plugins cannot replace the classifier" {
		t.Fatalf("err=%v", err)
	}
	if err := host.Register(sample{kind: KindExecutor}); err != nil {
		t.Fatal(err)
	}
	if got := host.List(); len(got) != 1 || got[0].Name != "sample" || got[0].Kind != KindExecutor {
		t.Fatalf("list=%+v", got)
	}
}
