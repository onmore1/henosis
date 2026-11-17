package flptest

import (
	"reflect"
	"testing"

	"henosis/flp"
)

func TestProjectIsReadable(t *testing.T) {
	samples := []string{`C:\Samples\kick.wav`, `/tmp/outside/snare.wav`}
	info, err := flp.Read(Project("Night Drive", samples, "v2"))
	if err != nil {
		t.Fatal(err)
	}
	if info.FLVersion != "25.1.6.4997" || info.Title != "Night Drive" || !reflect.DeepEqual(info.SamplePaths, samples) ||
		!reflect.DeepEqual(info.PluginNames(), []string{"Fruity Limiter"}) {
		t.Fatalf("%+v", info)
	}
	if plain, err := flp.Read(Project("", nil, "")); err != nil || plain.Title != "" || len(plain.SamplePaths) != 0 {
		t.Fatalf("%+v %v", plain, err)
	}
}
