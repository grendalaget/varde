package svc

import (
	"encoding/binary"
	"fmt"
	"sync"
)

// FrameInfo holds the parsed SVC metadata from a Dependency Descriptor header.
type FrameInfo struct {
	StartOfFrame bool
	EndOfFrame   bool
	TemplateID   uint8
	FrameNumber  uint16
	TemporalID   int // -1 if the template structure is unknown
	SpatialID    int // -1 if the template structure is unknown
}

// templateEntry maps a template_id to its spatial and temporal layer.
type templateEntry struct {
	spatialID  int
	temporalID int
}

// DDParser parses Dependency Descriptor RTP header extensions, keeping the template
// dependency structure so later 3-byte headers resolve to a layer. Safe for concurrent reads.
type DDParser struct {
	mu        sync.RWMutex
	templates map[uint8]templateEntry
}

// NewDDParser creates a parser with no template state.
func NewDDParser() *DDParser {
	return &DDParser{}
}

// Parse extracts FrameInfo from a raw DD header extension payload. At least 3 bytes; a
// template dependency structure, if present, is parsed and cached.
func (p *DDParser) Parse(data []byte) (FrameInfo, error) {
	if len(data) < 3 {
		return FrameInfo{}, fmt.Errorf("dd: payload too short (%d bytes)", len(data))
	}

	fi := FrameInfo{
		StartOfFrame: data[0]&0x80 != 0,
		EndOfFrame:   data[0]&0x40 != 0,
		TemplateID:   data[0] & 0x3F,
		FrameNumber:  binary.BigEndian.Uint16(data[1:3]),
		TemporalID:   -1,
		SpatialID:    -1,
	}

	// Longer than 3 bytes means a template dependency structure may follow. The first bit
	// after the mandatory header is what says so.
	if len(data) > 3 {
		hasTDS := data[3]&0x80 != 0
		if hasTDS {
			p.parseTemplateStructure(data[3:])
		}
	}

	p.mu.RLock()
	if t, ok := p.templates[fi.TemplateID]; ok {
		fi.TemporalID = t.temporalID
		fi.SpatialID = t.spatialID
	}
	p.mu.RUnlock()

	return fi, nil
}

// parseTemplateStructure parses the template dependency structure, starting at the byte
// holding the present flag. Bit layout: https://aomediacodec.github.io/av1-rtp-spec/
func (p *DDParser) parseTemplateStructure(data []byte) {
	if len(data) < 2 {
		return
	}

	r := bitReader{data: data, pos: 0}

	// template_dependency_structure_present_flag — skip (already known true)
	r.readBits(1)

	// active_decode_targets_present_flag
	activeDecodeTargetsPresent := r.readBits(1) != 0

	// template_id_offset (6 bits)
	templateIDOffset := uint8(r.readBits(6))

	// dt_cnt: number of decode targets (5 bits)
	dtCnt := r.readBits(5) + 1

	// Templates are listed until a repeated (spatial_id, temporal_id) pair matches the
	// first, or a safety limit is hit.
	type stPair struct{ s, t int }
	var templates []templateEntry
	var firstPair stPair
	firstSet := false

	for i := 0; i < 64; i++ {
		if r.err != nil || r.pos >= uint(len(data))*8 {
			break
		}
		spatialID := int(r.readBits(2))
		temporalID := int(r.readBits(3))

		pair := stPair{spatialID, temporalID}
		if !firstSet {
			firstPair = pair
			firstSet = true
		} else if pair == firstPair {
			// We've looped back to the first template — all templates parsed.
			break
		}

		templates = append(templates, templateEntry{
			spatialID:  spatialID,
			temporalID: temporalID,
		})
	}

	if len(templates) == 0 {
		return
	}

	// Skip the remaining fields (template DTIs, frame diffs, chains, etc.)
	// to avoid complex parsing. We only need the spatial/temporal mapping.
	_ = activeDecodeTargetsPresent
	_ = dtCnt

	// Build the template map.
	tplMap := make(map[uint8]templateEntry, len(templates))
	for i, t := range templates {
		id := templateIDOffset + uint8(i)
		tplMap[id] = t
	}

	p.mu.Lock()
	p.templates = tplMap
	p.mu.Unlock()
}

// HasTemplates returns true if the parser has received a template structure.
func (p *DDParser) HasTemplates() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.templates) > 0
}

// bitReader reads individual bits from a byte slice.
type bitReader struct {
	data []byte
	pos  uint // bit position
	err  error
}

func (b *bitReader) readBits(n uint) uint32 {
	if b.err != nil {
		return 0
	}
	var val uint32
	for i := uint(0); i < n; i++ {
		byteIdx := b.pos / 8
		bitIdx := 7 - (b.pos % 8)
		if int(byteIdx) >= len(b.data) {
			b.err = fmt.Errorf("dd: read past end at bit %d", b.pos)
			return val
		}
		if b.data[byteIdx]&(1<<bitIdx) != 0 {
			val |= 1 << (n - 1 - i)
		}
		b.pos++
	}
	return val
}
