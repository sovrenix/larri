// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package whisper

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"strconv"
	"time"

	"go.sovrenix.com/larri/internal/errs"
	"go.sovrenix.com/larri/internal/runtime"
)

// Client speaks the server's OpenAI audio API at the local end of the tunnel.
//
// The local end, deliberately, and for the reason readiness is checked there
// for every other workload: a call made on the host would prove only that the
// server answers itself, while a call through the tunnel proves the forward
// carries traffic, the proxy is substituting credentials, and a transcription
// comes back. What the operator is about to do is exactly what gets tested.
type Client struct {
	// Addr is host:port of the local listener.
	Addr string

	// Token authenticates to LARRI's proxy. It is a *client* token, never the
	// rig's: the proxy strips it and substitutes its own (FR-SEC-22).
	Token string

	// Probe marks traffic as LARRI's own so it does not reset the idle clock
	// (FR-SUP-08).
	Probe bool

	HTTP *http.Client
}

func (c *Client) base() string { return "http://" + c.Addr }

func (c *Client) do(ctx context.Context, method, path, contentType string,
	body []byte) ([]byte, int, error) {

	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base()+path, rdr)
	if err != nil {
		return nil, 0, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if c.Probe {
		req.Header.Set(runtime.ProbeHeader, "1")
	}
	cl := c.HTTP
	if cl == nil {
		// Generous: a long file is minutes of GPU work in a single request,
		// which is also why the idle clock does not need a holder for it.
		cl = &http.Client{Timeout: 10 * time.Minute}
	}
	resp, err := cl.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return raw, resp.StatusCode, nil
}

// Models lists what the server has loaded.
//
// Worth asking separately from "is it up", because the settings that choose
// the model are environment variables in someone else's image. A name this
// adapter got wrong does not fail: the server starts with its own default and
// transcribes happily with a model the operator did not ask for.
func (c *Client) Models(ctx context.Context) ([]string, error) {
	raw, code, err := c.do(ctx, http.MethodGet, "/v1/models", "", nil)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, errs.Newf(errs.ClassHostFailure, "whisper.Models", "http %d", code)
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, errs.Newf(errs.ClassHostFailure, "whisper.Models", "decode: %v", err)
	}
	out := make([]string, 0, len(body.Data))
	for _, m := range body.Data {
		out = append(out, m.ID)
	}
	return out, nil
}

// Transcribe posts one audio file and returns the text.
func (c *Client) Transcribe(ctx context.Context, wav []byte, filename string) (string, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)

	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition",
		`form-data; name="file"; filename="`+filename+`"`)
	h.Set("Content-Type", "audio/wav")
	part, err := mw.CreatePart(h)
	if err != nil {
		return "", err
	}
	if _, err := part.Write(wav); err != nil {
		return "", err
	}
	if err := mw.WriteField("response_format", "json"); err != nil {
		return "", err
	}
	if err := mw.Close(); err != nil {
		return "", err
	}

	raw, code, err := c.do(ctx, http.MethodPost, "/v1/audio/transcriptions",
		mw.FormDataContentType(), buf.Bytes())
	if err != nil {
		return "", err
	}
	if code != http.StatusOK {
		// A rejected request is the configuration's fault rather than the
		// host's: the next machine runs the same image and rejects it
		// identically (FR-PROV-05).
		return "", errs.Newf(errs.ClassModelFailure, "whisper.Transcribe",
			"http %d: %s", code, lastLine(string(raw)))
	}
	var body struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return "", errs.Newf(errs.ClassHostFailure, "whisper.Transcribe",
			"decode: %v", err)
	}
	return body.Text, nil
}

// Reachable reports whether anything is answering, without judging what.
func (c *Client) Reachable(ctx context.Context) error {
	_, code, err := c.do(ctx, http.MethodGet, "/v1/models", "", nil)
	if err != nil {
		return err
	}
	if code != http.StatusOK {
		return errs.Newf(errs.ClassHostFailure, "whisper.Reachable", "http %d", code)
	}
	return nil
}

// LocalAddr renders a host and port for Client.Addr.
func LocalAddr(host string, port int) string {
	return net.JoinHostPort(host, strconv.Itoa(port))
}

const (
	readySampleRate = 16000 // what whisper resamples everything to anyway
	readySeconds    = 5     // long enough to be a real window, short enough to be free
)

// ReadyClip is the audio readiness transcribes.
//
// Generated rather than shipped, which is the point: no clip in the
// repository, no licence attached to one, and no question about where a
// recording of a human voice came from.
//
// Noise rather than silence, also deliberately. The server's voice-activity
// filter can skip a silent file without running the model at all, and a
// readiness check that the VAD short-circuits proves the HTTP path and nothing
// else — which is exactly the class of false READY this project keeps paying
// for. What comes back is whatever the model hears in noise, and that is not
// asserted on; that it ran and answered is the claim.
//
// Deterministic, so two runs send identical bytes and a difference in the
// response is a difference in the rig.
func ReadyClip() []byte {
	n := readySampleRate * readySeconds
	samples := make([]int16, n)
	// A small xorshift, inline rather than math/rand, so the bytes cannot
	// change with a Go release.
	var state uint32 = 0x9E3779B9
	for i := range samples {
		state ^= state << 13
		state ^= state >> 17
		state ^= state << 5
		// Quiet: loud noise makes some models emit long hallucinations, and a
		// readiness check should not cost a minute of decoding.
		samples[i] = int16(int32(state&0x1FFF) - 0x1000)
	}
	return wavPCM16(samples, readySampleRate)
}

// wavPCM16 wraps samples in a canonical 44-byte RIFF header.
func wavPCM16(samples []int16, rate int) []byte {
	const (
		channels  = 1
		bits      = 16
		headerLen = 44
	)
	dataLen := len(samples) * 2
	buf := bytes.NewBuffer(make([]byte, 0, headerLen+dataLen))

	byteRate := rate * channels * bits / 8
	blockAlign := channels * bits / 8

	buf.WriteString("RIFF")
	_ = binary.Write(buf, binary.LittleEndian, uint32(36+dataLen))
	buf.WriteString("WAVE")

	buf.WriteString("fmt ")
	_ = binary.Write(buf, binary.LittleEndian, uint32(16)) // PCM chunk size
	_ = binary.Write(buf, binary.LittleEndian, uint16(1))  // PCM
	_ = binary.Write(buf, binary.LittleEndian, uint16(channels))
	_ = binary.Write(buf, binary.LittleEndian, uint32(rate))
	_ = binary.Write(buf, binary.LittleEndian, uint32(byteRate))
	_ = binary.Write(buf, binary.LittleEndian, uint16(blockAlign))
	_ = binary.Write(buf, binary.LittleEndian, uint16(bits))

	buf.WriteString("data")
	_ = binary.Write(buf, binary.LittleEndian, uint32(dataLen))
	for _, s := range samples {
		_ = binary.Write(buf, binary.LittleEndian, s)
	}
	return buf.Bytes()
}
