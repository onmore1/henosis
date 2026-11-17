package flp

import (
	"encoding/binary"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// --- helpers that build a project file in memory ---------------------------------

func utf16z(s string, extra ...uint16) []byte {
	var out []byte
	for _, r := range s {
		out = binary.LittleEndian.AppendUint16(out, uint16(r))
	}
	for _, u := range extra {
		out = binary.LittleEndian.AppendUint16(out, u)
	}
	return binary.LittleEndian.AppendUint16(out, 0)
}

func asciiz(s string) []byte { return append([]byte(s), 0) }

func eventVar(b []byte, id byte, value []byte) []byte {
	b = append(b, id)
	n := len(value)
	for {
		c := byte(n & 0x7F)
		n >>= 7
		if n != 0 {
			c |= 0x80
		}
		b = append(b, c)
		if n == 0 {
			break
		}
	}
	return append(b, value...)
}

func wrapperData(name, path, vendor string) []byte {
	d := binary.LittleEndian.AppendUint32(nil, 12)
	sub := func(id uint32, payload string) {
		d = binary.LittleEndian.AppendUint32(d, id)
		d = binary.LittleEndian.AppendUint64(d, uint64(len(payload)))
		d = append(d, payload...)
	}
	sub(1, string(make([]byte, 20)))
	sub(54, name)
	sub(55, path)
	sub(56, vendor)
	sub(53, string(make([]byte, 300))) // plugin state
	return d
}

func makeFile(events []byte) []byte {
	f := []byte("FLhd")
	f = binary.LittleEndian.AppendUint32(f, 6)
	f = binary.LittleEndian.AppendUint16(f, 0)  // format
	f = binary.LittleEndian.AppendUint16(f, 3)  // channels
	f = binary.LittleEndian.AppendUint16(f, 96) // ppq
	f = append(f, "FLdt"...)
	f = binary.LittleEndian.AppendUint32(f, uint32(len(events)))
	return append(f, events...)
}

func modernEvents() []byte {
	var e []byte
	e = eventVar(e, 199, asciiz("25.1.6.4997"))
	e = append(e, 28, 1)                       // a byte event
	e = append(e, 64, 0, 0)                    // a word event
	e = append(e, 128, 0x78, 0x56, 0x34, 0x12) // a dword event
	e = eventVar(e, 194, utf16z("My Song"))
	e = eventVar(e, 202, utf16z(`C:\Projects\My Song\`))
	e = eventVar(e, 201, utf16z(""))
	e = eventVar(e, 196, utf16z(`C:\Projects\My Song\Audio\vocal.wav`))
	e = eventVar(e, 196, utf16z(`%FLStudioFactoryData%\Data\Patches\Packs\Drums\Kick.wav`))
	e = eventVar(e, 196, utf16z(`D:\Samples\snare.wav`))
	e = eventVar(e, 196, utf16z(`D:\Samples\snare.wav`))
	e = eventVar(e, 201, utf16z("Fruity Limiter"))
	e = eventVar(e, 213, make([]byte, 52))
	e = eventVar(e, 201, utf16z("Fruity Limiter"))
	e = eventVar(e, 213, make([]byte, 52))
	e = eventVar(e, 201, utf16z("Fruity Wrapper"))
	e = eventVar(e, 212, make([]byte, 52))
	e = eventVar(e, 213, wrapperData("Serum", `C:\VST\Serum_x64.dll`, "Xfer Records"))
	e = eventVar(e, 201, utf16z("Fruity Wrapper"))
	e = eventVar(e, 213, wrapperData("Portal", `C:\VST3\Portal.vst3`, "Output"))
	return e
}

// --- tests ----------------------------------------------------------------------

func TestModernProject(t *testing.T) {
	info, err := Read(makeFile(modernEvents()))
	if err != nil {
		t.Fatal(err)
	}
	if info.Format != 0 || info.Channels != 3 || info.PPQ != 96 || info.EventCount != 20 {
		t.Errorf("header: %+v", info)
	}
	if info.FLVersion != "25.1.6.4997" || info.Title != "My Song" || info.DataPath != `C:\Projects\My Song\` {
		t.Errorf("text fields: %+v", info)
	}
	wantSamples := []string{`C:\Projects\My Song\Audio\vocal.wav`,
		`%FLStudioFactoryData%\Data\Patches\Packs\Drums\Kick.wav`, `D:\Samples\snare.wav`}
	if !reflect.DeepEqual(info.SamplePaths, wantSamples) {
		t.Errorf("samples: %q", info.SamplePaths)
	}
	if !reflect.DeepEqual(info.NativePlugins, []string{"Fruity Limiter"}) {
		t.Errorf("native: %q", info.NativePlugins)
	}
	wantVST := []VST{{"Serum", "Xfer Records", `C:\VST\Serum_x64.dll`}, {"Portal", "Output", `C:\VST3\Portal.vst3`}}
	if !reflect.DeepEqual(info.VSTPlugins, wantVST) || info.UnparsedVSTs != 0 {
		t.Errorf("vst: %+v unparsed %d", info.VSTPlugins, info.UnparsedVSTs)
	}
	if !reflect.DeepEqual(info.PluginNames(), []string{"Fruity Limiter", "Serum", "Portal"}) {
		t.Errorf("plugin names: %q", info.PluginNames())
	}
}

func TestTextEncodings(t *testing.T) {
	var e []byte
	e = eventVar(e, 199, asciiz("21.2.3.4004"))
	e = eventVar(e, 194, utf16z("H", 0x00E9, 0xD83C, 0xDFB5)) // "Hé" + U+1F3B5
	if info, err := Read(makeFile(e)); err != nil || info.Title != "H\u00e9\U0001F3B5" {
		t.Errorf("surrogate pair: %q %v", info.Title, err)
	}

	e = eventVar(nil, 199, asciiz("21.2.3.4004"))
	e = eventVar(e, 194, utf16z("A", 0xD83C)) // a lone surrogate
	if info, err := Read(makeFile(e)); err != nil || info.Title != "A\uFFFD" {
		t.Errorf("lone surrogate: %q %v", info.Title, err)
	}

	// Before FL Studio 11.5 text is 8-bit; 11.5 is the first UTF-16 version.
	e = eventVar(nil, 199, asciiz("9.0.3"))
	e = eventVar(e, 194, asciiz("Old Tune"))
	e = eventVar(e, 201, asciiz("3x Osc"))
	e = eventVar(e, 196, asciiz(`C:\Samples\clap.wav`))
	info, err := Read(makeFile(e))
	if err != nil || info.Title != "Old Tune" || len(info.NativePlugins) != 1 || len(info.SamplePaths) != 1 {
		t.Errorf("8-bit project: %+v %v", info, err)
	}
	e = eventVar(eventVar(nil, 199, asciiz("11.5.0")), 194, utf16z("Wide"))
	if info, _ := Read(makeFile(e)); info.Title != "Wide" {
		t.Errorf("11.5: %q", info.Title)
	}
	e = eventVar(eventVar(nil, 199, asciiz("11.1.0")), 194, asciiz("Narrow"))
	if info, _ := Read(makeFile(e)); info.Title != "Narrow" {
		t.Errorf("11.1: %q", info.Title)
	}
}

func TestUnreadableFiles(t *testing.T) {
	good := makeFile(modernEvents())
	version := eventVar(nil, 199, asciiz("25.1.6.4997"))
	mutate := func(at int, b byte) []byte {
		c := append([]byte(nil), good...)
		c[at] = b
		return c
	}
	bad := map[string][]byte{
		"empty":             {},
		"too small":         make([]byte, 10),
		"bad magic":         mutate(0, 'X'),
		"bad header size":   mutate(4, 7),
		"bad data magic":    mutate(14, 'X'),
		"truncated":         good[:len(good)-5],
		"event overrun":     makeFile(append(append([]byte(nil), version...), 213, 100)),
		"endless length":    makeFile(append(append([]byte(nil), version...), 213, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF)),
		"cut fixed event":   makeFile(append(append([]byte(nil), version...), 128, 1)),
		"no version":        makeFile(eventVar(nil, 194, utf16z("Song"))),
		"version not a num": makeFile(eventVar(nil, 199, asciiz("beta"))),
		"odd UTF-16":        makeFile(eventVar(append([]byte(nil), version...), 194, []byte{'A', 0, 'B'})),
	}
	for name, data := range bad {
		if _, err := Read(data); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// Bytes after the data chunk are ignored.
	if _, err := Read(append(append([]byte(nil), good...), 0xAA, 0xBB)); err != nil {
		t.Errorf("trailing bytes: %v", err)
	}
}

func TestUnparsedWrapperIsCounted(t *testing.T) {
	e := eventVar(nil, 199, asciiz("25.1.6.4997"))
	e = eventVar(e, 201, utf16z("Fruity Wrapper"))
	e = eventVar(e, 213, []byte{1, 2, 3}) // too short for a type
	e = eventVar(e, 201, utf16z("Fruity Wrapper"))
	broken := wrapperData("Serum", "x", "y")
	e = eventVar(e, 213, broken[:len(broken)-10]) // the last sub-event overruns
	e = eventVar(e, 201, utf16z("Fruity Wrapper"))
	nameless := binary.LittleEndian.AppendUint32(nil, 12)
	nameless = binary.LittleEndian.AppendUint32(nameless, 55)
	nameless = binary.LittleEndian.AppendUint64(nameless, 3)
	nameless = append(nameless, "abc"...)
	e = eventVar(e, 213, nameless) // well-formed but no name
	info, err := Read(makeFile(e))
	if err != nil || len(info.VSTPlugins) != 0 || info.UnparsedVSTs != 3 {
		t.Errorf("got %+v, %v", info, err)
	}
}

func TestClassifySample(t *testing.T) {
	folder := `C:\Users\me\Documents\Henosis Projects\My Song`
	cases := []struct {
		sample, folder string
		want           SampleLocation
	}{
		{folder + `\Audio\a.wav`, folder, InsideProject},
		{"c:/users/ME/documents/henosis projects/my song/audio/a.wav", folder, InsideProject},
		{folder, folder + `\`, Outside},
		{folder + ` 2\a.wav`, folder, Outside}, // a sibling that only starts with the same name
		{`D:\Samples\snare.wav`, folder, Outside},
		{`%FLStudioFactoryData%\Data\Patches\Kick.wav`, folder, Factory},
		{`D:\Samples\snare.wav`, "", Outside},
	}
	for _, c := range cases {
		if got := ClassifySample(c.sample, c.folder); got != c.want {
			t.Errorf("ClassifySample(%q) = %v, want %v", c.sample, got, c.want)
		}
	}
}

// The reader must reject damaged input without panicking.
func TestDamagedInputNeverPanics(t *testing.T) {
	good := makeFile(modernEvents())
	for n := 0; n <= len(good); n++ {
		Read(good[:n])
	}
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 20000; i++ {
		m := append([]byte(nil), good...)
		for k := 0; k <= rng.Intn(8); k++ {
			m[rng.Intn(len(m))] = byte(rng.Intn(256))
		}
		if rng.Intn(4) == 0 {
			m = m[:rng.Intn(len(m))]
		}
		Read(m)
	}
}

// Optional: real projects. Set HENOSIS_TEST_FLP_DIR to a folder of .flp files.
func TestRealProjects(t *testing.T) {
	dir := os.Getenv("HENOSIS_TEST_FLP_DIR")
	if dir == "" {
		t.Skip("HENOSIS_TEST_FLP_DIR is not set")
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*", "*.flp"))
	if len(files) == 0 {
		t.Fatalf("no .flp files under %s", dir)
	}
	for _, file := range files {
		info, err := ReadFile(file)
		if err != nil {
			t.Errorf("%s: %v", file, err)
			continue
		}
		t.Logf("%s: FL %s, %d events, %d native, %d VST, %d samples, %d unparsed",
			filepath.Base(file), info.FLVersion, info.EventCount, len(info.NativePlugins),
			len(info.VSTPlugins), len(info.SamplePaths), info.UnparsedVSTs)
		if info.FLVersion == "" || info.UnparsedVSTs != 0 {
			t.Errorf("%s: unexpected result %+v", file, info)
		}
	}
}
