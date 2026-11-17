// Package flp is a read-only parser for FL Studio project files.
//
// Image-Line does not publish the format. The layout follows the PyFLP
// documentation and was checked against projects saved by FL Studio 25.1.6.
// See "Desktop client > Reading .flp files" in the technical docs.
package flp

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"strings"
	"unicode/utf16"
)

// Event IDs, named as in PyFLP.
const (
	eventTitle        = 194 // ProjectID.Title
	eventSamplePath   = 196 // ChannelID.SamplePath
	eventVersion      = 199 // ProjectID.FLVersion
	eventInternalName = 201 // PluginID.InternalName
	eventDataPath     = 202 // ProjectID.DataPath
	eventPluginData   = 213 // PluginID.Data

	vstSubName   = 54
	vstSubPath   = 55
	vstSubVendor = 56

	eventsStart = 22 // 14-byte header chunk + "FLdt" + size
	maxFileSize = 256 << 20

	vstWrapperName     = "Fruity Wrapper"
	factoryPlaceholder = "%flstudiofactorydata%"
)

// VST is a plugin hosted through FL Studio's wrapper (VST2, VST3, ...).
type VST struct {
	Name   string
	Vendor string
	Path   string // where the plugin is installed on the saving machine
}

// Info is everything Henosis needs from a project file.
type Info struct {
	Format   int16
	Channels uint16
	PPQ      uint16

	FLVersion string // for example "25.1.6.4997"
	Title     string // from the project info; often empty
	DataPath  string // the project data folder, as stored

	SamplePaths   []string // unique, in file order
	NativePlugins []string // unique internal names
	VSTPlugins    []VST    // unique

	// Wrapper plugins whose data did not have the expected layout. They are
	// counted, never guessed.
	UnparsedVSTs int
	EventCount   int
}

// PluginNames lists every plugin the project uses, once each, in file order.
func (i Info) PluginNames() []string {
	seen := map[string]bool{}
	var names []string
	add := func(name string) {
		if name != "" && !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	for _, name := range i.NativePlugins {
		add(name)
	}
	for _, vst := range i.VSTPlugins {
		add(vst.Name)
	}
	return names
}

type event struct {
	id    byte
	value []byte
}

// Read parses a project held in memory. It never reads outside data. If the
// event stream is inconsistent in any way the result is an error, not a
// partial Info.
func Read(data []byte) (Info, error) {
	if len(data) < eventsStart {
		return Info{}, errors.New("the file is too small to be a project")
	}
	if string(data[0:4]) != "FLhd" {
		return Info{}, errors.New("missing FLhd header")
	}
	if binary.LittleEndian.Uint32(data[4:8]) != 6 {
		return Info{}, errors.New("unexpected header size")
	}
	if string(data[14:18]) != "FLdt" {
		return Info{}, errors.New("missing FLdt chunk")
	}
	dataSize := uint64(binary.LittleEndian.Uint32(data[18:22]))
	if dataSize > uint64(len(data)-eventsStart) {
		return Info{}, errors.New("the data chunk is longer than the file")
	}
	end := eventsStart + int(dataSize)

	// Pass 1: split the data chunk into events without interpreting them.
	var events []event
	pos := eventsStart
	for pos < end {
		id := data[pos]
		pos++
		var size uint64
		switch {
		case id < 64:
			size = 1
		case id < 128:
			size = 2
		case id < 192:
			size = 4
		default:
			// Length prefix: 7 bits per byte, least significant group first,
			// top bit set on every byte but the last.
			shift := uint(0)
			for {
				if pos >= end {
					return Info{}, errors.New("an event length is cut off")
				}
				b := data[pos]
				pos++
				size |= uint64(b&0x7F) << shift
				if b&0x80 == 0 {
					break
				}
				shift += 7
				if shift > 28 {
					return Info{}, errors.New("an event length is too large")
				}
			}
		}
		if size > uint64(end-pos) {
			return Info{}, errors.New("an event runs past the end of the data chunk")
		}
		events = append(events, event{id: id, value: data[pos : pos+int(size)]})
		pos += int(size)
	}

	info := Info{
		Format:     int16(binary.LittleEndian.Uint16(data[8:10])),
		Channels:   binary.LittleEndian.Uint16(data[10:12]),
		PPQ:        binary.LittleEndian.Uint16(data[12:14]),
		EventCount: len(events),
	}

	// The version string is always 8-bit and decides how other text is read.
	versionFound := false
	for _, e := range events {
		if e.id == eventVersion {
			info.FLVersion = decode8Bit(e.value)
			versionFound = true
			break
		}
	}
	if !versionFound {
		return Info{}, errors.New("no FL Studio version in the file")
	}
	wide, ok := versionUsesUTF16(info.FLVersion)
	if !ok {
		return Info{}, errors.New("the FL Studio version is not readable")
	}
	text := func(v []byte) (string, error) {
		if !wide {
			return decode8Bit(v), nil
		}
		if len(v)%2 != 0 {
			return "", errors.New("a text event has an odd number of bytes")
		}
		return decodeUTF16LE(v), nil
	}

	// Pass 2: pick out the few events Henosis uses.
	titleSeen, dataPathSeen, wrapperPending := false, false, false
	seenSample, seenNative, seenVST := map[string]bool{}, map[string]bool{}, map[VST]bool{}
	for _, e := range events {
		switch e.id {
		case eventTitle:
			if !titleSeen {
				s, err := text(e.value)
				if err != nil {
					return Info{}, err
				}
				info.Title, titleSeen = s, true
			}
		case eventDataPath:
			if !dataPathSeen {
				s, err := text(e.value)
				if err != nil {
					return Info{}, err
				}
				info.DataPath, dataPathSeen = s, true
			}
		case eventSamplePath:
			s, err := text(e.value)
			if err != nil {
				return Info{}, err
			}
			if s != "" && !seenSample[s] {
				seenSample[s] = true
				info.SamplePaths = append(info.SamplePaths, s)
			}
		case eventInternalName:
			s, err := text(e.value)
			if err != nil {
				return Info{}, err
			}
			wrapperPending = s == vstWrapperName
			if !wrapperPending && s != "" && !seenNative[s] {
				seenNative[s] = true
				info.NativePlugins = append(info.NativePlugins, s)
			}
		case eventPluginData:
			if wrapperPending {
				if vst, ok := parseWrapperData(e.value); ok {
					if !seenVST[vst] {
						seenVST[vst] = true
						info.VSTPlugins = append(info.VSTPlugins, vst)
					}
				} else {
					info.UnparsedVSTs++
				}
				wrapperPending = false
			}
		}
	}
	return info, nil
}

// ReadFile reads and parses a project file. Files above 256 MiB are refused.
func ReadFile(path string) (Info, error) {
	stat, err := os.Stat(path)
	if err != nil {
		return Info{}, err
	}
	if stat.Size() > maxFileSize {
		return Info{}, errors.New("the file is larger than 256 MiB")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Info{}, err
	}
	info, err := Read(data)
	if err != nil {
		return Info{}, fmt.Errorf("unreadable project file: %w", err)
	}
	return info, nil
}

// The data of a wrapper plugin: a 32-bit type, then sub-events made of a
// 32-bit id, a 64-bit length and the payload. ok is false when the bytes do
// not have exactly that shape or carry no plugin name.
func parseWrapperData(v []byte) (VST, bool) {
	var vst VST
	if len(v) < 4 {
		return vst, false
	}
	pos := 4
	for pos < len(v) {
		if len(v)-pos < 12 {
			return VST{}, false
		}
		id := binary.LittleEndian.Uint32(v[pos:])
		length := binary.LittleEndian.Uint64(v[pos+4:])
		pos += 12
		if length > uint64(len(v)-pos) {
			return VST{}, false
		}
		payload := strings.TrimRight(string(v[pos:pos+int(length)]), "\x00")
		pos += int(length)
		switch id {
		case vstSubName:
			vst.Name = payload
		case vstSubPath:
			vst.Path = payload
		case vstSubVendor:
			vst.Vendor = payload
		}
	}
	return vst, vst.Name != ""
}

// UTF-16LE to UTF-8. Trailing NUL terminators are dropped; a lone surrogate
// becomes U+FFFD.
func decodeUTF16LE(v []byte) string {
	units := make([]uint16, len(v)/2)
	for i := range units {
		units[i] = binary.LittleEndian.Uint16(v[2*i:])
	}
	for len(units) > 0 && units[len(units)-1] == 0 {
		units = units[:len(units)-1]
	}
	return string(utf16.Decode(units))
}

// 8-bit text (projects saved before FL Studio 11.5), read as Latin-1.
func decode8Bit(v []byte) string {
	for len(v) > 0 && v[len(v)-1] == 0 {
		v = v[:len(v)-1]
	}
	runes := make([]rune, len(v))
	for i, b := range v {
		runes[i] = rune(b)
	}
	return string(runes)
}

// Text events are UTF-16LE from FL Studio 11.5 on and 8-bit before that.
// ok is false when the version does not start with a number.
func versionUsesUTF16(version string) (wide bool, ok bool) {
	pos := 0
	number := func() (int, bool) {
		value, any := 0, false
		for pos < len(version) && version[pos] >= '0' && version[pos] <= '9' {
			value = value*10 + int(version[pos]-'0')
			if value > 1_000_000 {
				return 0, false
			}
			any = true
			pos++
		}
		return value, any
	}
	major, ok := number()
	if !ok {
		return false, false
	}
	minor := 0
	if pos < len(version) && version[pos] == '.' {
		pos++
		minor, _ = number()
	}
	return major > 11 || (major == 11 && minor >= 5), true
}

// SampleLocation says where a sample lives relative to a project folder.
type SampleLocation int

const (
	Factory       SampleLocation = iota // ships with FL Studio; every install has it
	InsideProject                       // under the project folder, so it is synced
	Outside                             // somewhere else on disk; a partner will not have it
)

// ClassifySample classifies a path from Info.SamplePaths. The comparison
// ignores case and treats '/' and '\' as the same separator.
func ClassifySample(samplePath, projectFolder string) SampleLocation {
	sample := normalise(samplePath)
	if strings.Contains(sample, factoryPlaceholder) {
		return Factory
	}
	folder := normalise(projectFolder)
	if folder != "" && strings.HasPrefix(sample, folder+"/") {
		return InsideProject
	}
	return Outside
}

func normalise(path string) string {
	path = strings.ToLower(strings.ReplaceAll(path, `\`, "/"))
	return strings.TrimRight(path, "/")
}
