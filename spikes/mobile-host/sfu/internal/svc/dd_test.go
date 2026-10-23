package svc

import (
	"testing"
)

func TestParseMandatoryOnly(t *testing.T) {
	p := NewDDParser()

	// 3-byte mandatory header: start_of_frame=1, end_of_frame=1, template_id=5, frame_number=0x0102
	data := []byte{0xC5, 0x01, 0x02}
	fi, err := p.Parse(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !fi.StartOfFrame {
		t.Error("expected StartOfFrame=true")
	}
	if !fi.EndOfFrame {
		t.Error("expected EndOfFrame=true")
	}
	if fi.TemplateID != 5 {
		t.Errorf("expected TemplateID=5, got %d", fi.TemplateID)
	}
	if fi.FrameNumber != 0x0102 {
		t.Errorf("expected FrameNumber=0x0102, got %d", fi.FrameNumber)
	}
	if fi.TemporalID != -1 {
		t.Errorf("expected TemporalID=-1 (unknown), got %d", fi.TemporalID)
	}
}

func TestParseTooShort(t *testing.T) {
	p := NewDDParser()
	_, err := p.Parse([]byte{0x00, 0x00})
	if err == nil {
		t.Error("expected error for 2-byte payload")
	}
}

func TestParseWithTemplateStructure(t *testing.T) {
	p := NewDDParser()

	// A synthetic DD payload carrying an L1T3 template structure, worked out against
	// https://aomediacodec.github.io/av1-rtp-spec/ — bytes 3 to 6 are 0x80 0x00 0x02 0x40.

	// The same TDS portion, bit by bit: byte 3 0x80, byte 4 0x00, byte 5 0x02, byte 6 0x20.
	// See the spec link in dd.go for what each field is.
	data := []byte{
		0xC0, 0x00, 0x00, // mandatory: start=1 end=1 template_id=0 frame=0
		0x80, // TDS present, no active decode targets, offset=0
		0x00, // dtCnt-1=0, T0: spatial=00, temporal MSB=0
		0x02, // T0 temporal done=00, T1: spatial=00, temporal=001, T2 spatial MSB=0
		0x20, // T2 spatial LSB=0, T2 temporal=010, end: spatial=00, temporal=000
		0x00, // end temporal LSB + pad
	}

	fi, err := p.Parse(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if fi.TemplateID != 0 {
		t.Errorf("expected TemplateID=0, got %d", fi.TemplateID)
	}

	// Template 0 should map to temporal_id=0
	if fi.TemporalID != 0 {
		t.Errorf("expected TemporalID=0, got %d", fi.TemporalID)
	}

	// Now verify subsequent packets with just the mandatory header resolve correctly
	testCases := []struct {
		templateID     uint8
		wantTemporalID int
	}{
		{0, 0},
		{1, 1},
		{2, 2},
	}

	for _, tc := range testCases {
		pkt := []byte{tc.templateID, 0x00, 0x01}
		fi2, err := p.Parse(pkt)
		if err != nil {
			t.Fatalf("unexpected error for template_id=%d: %v", tc.templateID, err)
		}
		if fi2.TemporalID != tc.wantTemporalID {
			t.Errorf("template_id=%d: want temporalID=%d, got %d", tc.templateID, tc.wantTemporalID, fi2.TemporalID)
		}
	}
}

func TestBitReader(t *testing.T) {
	data := []byte{0b11010110, 0b01011010}
	r := bitReader{data: data, pos: 0}

	// Read 3 bits: 110
	v := r.readBits(3)
	if v != 6 {
		t.Errorf("expected 6 (110), got %d", v)
	}

	// Read 5 bits: 10110
	v = r.readBits(5)
	if v != 22 {
		t.Errorf("expected 22 (10110), got %d", v)
	}

	// Read 8 bits: 01011010
	v = r.readBits(8)
	if v != 0x5A {
		t.Errorf("expected 0x5A, got 0x%02X", v)
	}
}

func TestHasTemplates(t *testing.T) {
	p := NewDDParser()
	if p.HasTemplates() {
		t.Error("expected no templates initially")
	}
}
