package stream

import (
	"strings"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

func testForwarder(t *testing.T, clock float64, t0 time.Time) *forwarder {
	t.Helper()
	track, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: uint32(clock)}, "video", "teste")
	if err != nil {
		t.Fatal(err)
	}
	return &forwarder{track: track, clock: clock, t0: t0}
}

// pkt simula um pacote vindo do FFmpeg (o forwarder reescreve o próprio pacote).
func pkt(seq uint16, ts uint32, marker bool, size int) *rtp.Packet {
	return &rtp.Packet{
		Header:  rtp.Header{Version: 2, SSRC: 1234, SequenceNumber: seq, Timestamp: ts, Marker: marker},
		Payload: make([]byte, size),
	}
}

func TestForwarderSequenceIsContinuousAcrossRestarts(t *testing.T) {
	f := testForwarder(t, 90000, time.Now())
	f.newSess = true
	var got []uint16
	for _, s := range []uint16{500, 501, 502} { // primeira execução do FFmpeg
		p := pkt(s, 1000, false, 100)
		f.write(p)
		got = append(got, p.SequenceNumber)
	}
	f.newSess = true // FFmpeg reiniciou (ex.: trocou a resolução): seq recomeça em outro número
	for _, s := range []uint16{9, 10} {
		p := pkt(s, 77, false, 100)
		f.write(p)
		got = append(got, p.SequenceNumber)
	}
	for i, s := range got {
		if s != uint16(i) {
			t.Fatalf("sequência deveria ser 0,1,2,3,4 sem buracos nem repetição; veio %v", got)
		}
	}
}

func TestForwarderSequenceWraps(t *testing.T) {
	f := testForwarder(t, 90000, time.Now())
	f.newSess = true
	f.seq = 65535
	a, b := pkt(1, 0, false, 10), pkt(2, 0, false, 10)
	f.write(a)
	f.write(b)
	if a.SequenceNumber != 65535 || b.SequenceNumber != 0 {
		t.Errorf("depois de 65535 vem 0 (o RTP dá a volta): %d, %d", a.SequenceNumber, b.SequenceNumber)
	}
}

func TestForwarderKeepsTimestampDeltas(t *testing.T) {
	f := testForwarder(t, 90000, time.Now())
	f.newSess = true
	// timestamps do FFmpeg perto do limite de 32 bits: a diferença tem que sobreviver à volta
	src := []uint32{4294967295 - 1500, 4294967295 - 0, 1499} // passos de 1500 (60 fps a 90 kHz)
	var out []uint32
	for _, ts := range src {
		p := pkt(0, ts, true, 10)
		f.write(p)
		out = append(out, p.Timestamp)
	}
	for i := 1; i < len(out); i++ {
		if d := out[i] - out[i-1]; d != 1500 {
			t.Errorf("intervalo entre quadros mudou: %d, quero 1500 (saída %v)", d, out)
		}
	}
}

func TestForwarderTimestampFollowsClockAfterRestart(t *testing.T) {
	// a transmissão começou há 2 s; o FFmpeg reinicia com timestamps aleatórios
	t0 := time.Now().Add(-2 * time.Second)
	f := testForwarder(t, 90000, t0)
	f.newSess = true
	first := pkt(0, 123456789, false, 10)
	f.write(first)
	// a base nova é o tempo desde t0 no relógio RTP: ~2 s × 90 kHz = 180000
	if first.Timestamp < 175000 || first.Timestamp > 200000 {
		t.Errorf("timestamp após reinício = %d, esperado ~180000 (tempo real desde o início)", first.Timestamp)
	}

	time.Sleep(50 * time.Millisecond)
	f.newSess = true
	second := pkt(0, 42, false, 10)
	f.write(second)
	if int32(second.Timestamp-first.Timestamp) <= 0 {
		t.Errorf("timestamp voltou no tempo após reinício: %d → %d (quem assiste congelaria)", first.Timestamp, second.Timestamp)
	}
}

func TestForwarderCountsBytesAndFrames(t *testing.T) {
	f := testForwarder(t, 48000, time.Now())
	f.newSess = true
	f.write(pkt(0, 0, false, 1000))
	f.write(pkt(1, 0, true, 500)) // marker = último pacote de um quadro
	f.write(pkt(2, 0, true, 88))
	if got, want := f.bytes.Load(), uint64(1000+500+88+3*12); got != want { // + 12 bytes de cabeçalho RTP cada
		t.Errorf("bytes = %d, quero %d", got, want)
	}
	if got := f.frames.Load(); got != 2 {
		t.Errorf("quadros = %d, quero 2", got)
	}
}

func TestPacerDoesNotDelayNormalTraffic(t *testing.T) {
	p := &pacer{last: time.Now().Add(-time.Second)} // balde cheio
	p.setKbps(12000)
	start := time.Now()
	for range 40 { // ~48 KB: um quadro comum, cabe no balde
		p.wait(1200)
	}
	if d := time.Since(start); d > 20*time.Millisecond {
		t.Errorf("tráfego normal foi atrasado em %v", d)
	}
}

func TestPacerSpreadsBursts(t *testing.T) {
	p := &pacer{last: time.Now().Add(-time.Second)}
	p.setKbps(8000) // 3 × 8 Mbps = 3 MB/s; balde de 64 KB
	start := time.Now()
	for range 300 { // quadro-chave de ~360 KB: 64 KB saem na hora, ~296 KB a 3 MB/s ≈ 100 ms
		p.wait(1200)
	}
	d := time.Since(start)
	if d < 70*time.Millisecond {
		t.Errorf("rajada de 360 KB saiu em %v: o pacer não espalhou", d)
	}
	if d > time.Second {
		t.Errorf("rajada demorou %v: o pacer está segurando demais", d)
	}
}

func TestTailWriterKeepsLastLines(t *testing.T) {
	var w tailWriter
	w.Write([]byte("linha 1\nlinha 2\n\nlinha 3\nlinha"))
	w.Write([]byte(" 4\nlinha 5\nmeio da"))
	got := w.String()
	if want := "linha 2 | linha 3 | linha 4 | linha 5 | meio da"; got != want {
		t.Errorf("tail = %q, quero %q", got, want)
	}
	if strings.Contains(got, "linha 1") {
		t.Error("deveria guardar só as 4 últimas linhas completas")
	}
}
