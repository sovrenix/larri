// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package comfyui

import (
	"context"
	"fmt"
	"path"
	"strings"

	"go.sovrenix.com/larri/internal/claw/comfyui/workflow"
	"go.sovrenix.com/larri/internal/errs"
	"go.sovrenix.com/larri/internal/runtime"
	"go.sovrenix.com/larri/internal/secret"
)

// Download plans, launches and observes the model fetch on the host.
//
// Weights are pulled by the *host*, not relayed through the operator's link,
// and that is the only arrangement that makes sense: the files are tens of
// gigabytes, the host has a datacentre link that ranking already selected for
// (§4b), and the operator's upstream is typically a fraction of it. Relaying
// would also pay for the same bytes twice — once to arrive locally, once to
// leave — while the rig sat idle and billing.
type Download struct {
	// Dir is ComfyUI's models root on the host.
	Dir string

	// Items are what to fetch. Order is preserved, largest first is the
	// caller's choice to make.
	Items []Item

	// Log is where the fetch script writes, so progress is readable by the
	// same stall detection that watches a runtime's log.
	Log string
}

// Item is one file to place on the host.
type Item struct {
	// URL is where the bytes come from.
	URL string

	// Kind is the models/ subdirectory ComfyUI expects it in.
	Kind workflow.Kind

	// Name is the filename ComfyUI resolves, subdirectories included.
	Name string

	// Bytes is the expected size, used to verify the download rather than to
	// trust it. A truncated checkpoint fails at load with a stack trace that
	// says nothing about the network.
	Bytes uint64
}

// Dest is where this item lands on the host.
func (i Item) Dest(root string) string {
	return path.Join(root, string(i.Kind), i.Name)
}

// ModelsDir is where the adapter keeps ComfyUI's models.
const ModelsDir = "/opt/ComfyUI/models"

// FetchLog is the fetch script's output, read for progress and for diagnosis.
const FetchLog = "/var/log/larri-comfy-fetch.log"

// fetchDone is written only after every file has arrived and verified. Its
// existence is the completion signal, rather than the script's exit status,
// which a detached process does not report back anywhere.
const fetchDone = "/var/log/larri-comfy-fetch.done"

// curlConfig holds the Hugging Face credential.
//
// A curl config file rather than a command-line header, and the difference is
// not cosmetic: an -H argument puts the token in the process table, in the
// shell's history, and — the one that actually bites — in the command string
// LARRI itself builds, which is the sort of thing that ends up in a debug log
// next to the error it was meant to explain. FR-SEC and invariant 9 both say a
// credential is never echoed; a config file is how that is kept true while
// still authenticating a gated repository.
const curlConfig = "/root/.larri-comfy-curl"

// SafeName reports whether an asset name may be written under the models root.
//
// The name comes out of a workflow file, which is an artefact operators
// download from strangers and open without reading. A graph naming its
// checkpoint "../../../root/.ssh/authorized_keys" is not a hypothetical shape
// of attack; it is the oldest one there is, and the fetch runs as root.
func SafeName(name string) error {
	if name == "" {
		return errs.Newf(errs.ClassModelFailure, "comfy.SafeName", "empty model name")
	}
	if strings.HasPrefix(name, "/") || strings.HasPrefix(name, "\\") {
		return errs.Newf(errs.ClassModelFailure, "comfy.SafeName",
			"absolute model path %q", name)
	}
	if strings.Contains(name, "\x00") {
		return errs.Newf(errs.ClassModelFailure, "comfy.SafeName",
			"model name contains a null byte")
	}
	for _, seg := range strings.FieldsFunc(name, func(r rune) bool { return r == '/' || r == '\\' }) {
		if seg == ".." {
			return errs.Newf(errs.ClassModelFailure, "comfy.SafeName",
				"model path escapes the models directory: %q", name)
		}
	}
	return nil
}

// shellQuote renders s as a single POSIX shell word.
//
// Every value interpolated into a host command passes through here. The inputs
// are filenames and URLs taken from a workflow and a manifest, neither of
// which LARRI wrote, and the commands run as root on a machine that is holding
// the operator's Hugging Face token.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// WriteCredential places the Hugging Face token on the host.
//
// Issued as its own command so the caller can keep it out of whatever it logs;
// nothing else in the fetch ever names the token.
func WriteCredential(ctx context.Context, sess runtime.Session, token secret.Secret) error {
	if token.Empty() {
		// Nothing to write, and the absence is not an error: most ComfyUI
		// models are public, and a token is only needed for a gated
		// repository, which resolution has already proven readable.
		_, _ = sess.Run(ctx, "rm -f "+shellQuote(curlConfig))
		return nil
	}
	cmd := fmt.Sprintf(
		`umask 077 && printf '%%s\n' %s > %s`,
		shellQuote("header = \"Authorization: Bearer "+token.Reveal()+"\""),
		shellQuote(curlConfig))
	if _, err := sess.Run(ctx, cmd); err != nil {
		return errs.Newf(errs.ClassHostFailure, "comfy.WriteCredential",
			"write the fetch credential: %v", err)
	}
	return nil
}

// Script builds the fetch script.
//
// Sequential rather than parallel, and that is a considered choice on a billed
// link. Parallel downloads do not make a saturated connection faster; what
// they do is make the progress signal useless, because four half-finished
// files on disk look exactly like one stalled one. A rig is torn down on
// evidence of a stall (§12.2.1), so a fetch that cannot be observed clearly is
// a fetch that gets a working host destroyed.
func (d Download) Script(useCredential bool) (string, error) {
	var b strings.Builder
	b.WriteString("set -u\n")
	b.WriteString("rm -f " + shellQuote(fetchDone) + "\n")

	conf := ""
	if useCredential {
		// -K is read only if it exists, so a rig with no token still runs
		// this script unchanged.
		conf = "-K " + shellQuote(curlConfig) + " "
	}

	for _, it := range d.Items {
		if err := SafeName(it.Name); err != nil {
			return "", err
		}
		dest := it.Dest(d.Dir)
		dir := path.Dir(dest)
		part := dest + ".part"

		b.WriteString("mkdir -p " + shellQuote(dir) + "\n")
		// Skip what is already there and the right size. A rig replaced
		// underneath a live session, or a fetch resumed after a dropped
		// connection, must not pay for the same 7 GB twice.
		b.WriteString(fmt.Sprintf(
			"if [ -f %s ] && [ \"$(stat -c %%s %s 2>/dev/null || echo 0)\" = %s ]; then\n"+
				"  echo \"have %s\"\n"+
				"else\n",
			shellQuote(dest), shellQuote(dest), shellQuote(fmt.Sprint(it.Bytes)),
			it.Name))
		b.WriteString("  echo \"fetch " + it.Name + "\"\n")
		// -f so an HTML error page is not written out as a checkpoint, -L to
		// follow the redirect to the CDN, -C - to resume a partial file.
		b.WriteString(fmt.Sprintf(
			"  curl -fL -C - --retry 5 --retry-delay 5 --retry-connrefused "+
				"%s-o %s %s || { echo \"FAILED %s\"; exit 1; }\n",
			conf, shellQuote(part), shellQuote(it.URL), it.Name))
		if it.Bytes > 0 {
			// Verify before the rename, so a truncated file is never visible
			// to ComfyUI under its real name.
			b.WriteString(fmt.Sprintf(
				"  got=$(stat -c %%s %s 2>/dev/null || echo 0)\n"+
					"  if [ \"$got\" != %s ]; then echo \"SHORT %s $got != %d\"; exit 1; fi\n",
				shellQuote(part), shellQuote(fmt.Sprint(it.Bytes)), it.Name, it.Bytes))
		}
		b.WriteString("  mv " + shellQuote(part) + " " + shellQuote(dest) + "\n")
		b.WriteString("fi\n")
	}
	b.WriteString("echo ALLDONE\n")
	b.WriteString("touch " + shellQuote(fetchDone) + "\n")
	return b.String(), nil
}

// Start writes the script and launches it detached.
//
// Detached because the fetch outlives any sensible SSH command timeout — tens
// of gigabytes at a host's link speed — and a session that dropped mid-fetch
// would otherwise kill it and throw away everything already on disk, which is
// the same mistake the fixed provisioning deadline made (§12.2.1).
func (d Download) Start(ctx context.Context, sess runtime.Session, useCredential bool) error {
	script, err := d.Script(useCredential)
	if err != nil {
		return err
	}
	logPath := d.Log
	if logPath == "" {
		logPath = FetchLog
	}
	// A heredoc with a quoted delimiter, so nothing in the script is expanded
	// by the shell that writes it.
	const scriptPath = "/root/.larri-comfy-fetch.sh"
	write := fmt.Sprintf("cat > %s <<'LARRI_FETCH_EOF'\n%s\nLARRI_FETCH_EOF\nchmod 700 %s",
		shellQuote(scriptPath), script, shellQuote(scriptPath))
	if _, err := sess.Run(ctx, write); err != nil {
		return errs.Newf(errs.ClassHostFailure, "comfy.Download",
			"write the fetch script: %v", err)
	}
	// setsid and </dev/null for the same reason the server launch needs them:
	// sshd holds an exec channel open while any live process still has a
	// descriptor on it, and stdin is that channel. This one happened to
	// return anyway, which makes it the more dangerous of the two — a latent
	// hang that works until the script outlives the session.
	launch := fmt.Sprintf("setsid nohup sh %s </dev/null >%s 2>&1 & echo STARTED",
		shellQuote(scriptPath), shellQuote(logPath))
	out, err := sess.Run(ctx, launch)
	if err != nil {
		return errs.Newf(errs.ClassHostFailure, "comfy.Download",
			"start the fetch: %v", err)
	}
	if !strings.Contains(string(out), "STARTED") {
		return errs.Newf(errs.ClassHostFailure, "comfy.Download",
			"start the fetch: no confirmation from the host")
	}
	return nil
}

// Done reports whether the fetch finished, and fails loudly if it stopped
// badly.
//
// The marker file is the signal rather than the log's last line, because a log
// can end mid-word when a host is killed and "ALLDONE" is a string an error
// message could contain. A file that exists only after a verified rename
// cannot be faked by a truncated write.
func (d Download) Done(ctx context.Context, sess runtime.Session) (bool, error) {
	out, _ := sess.Run(ctx, "test -f "+shellQuote(fetchDone)+" && echo YES || echo NO")
	if strings.Contains(string(out), "YES") {
		return true, nil
	}
	logPath := d.Log
	if logPath == "" {
		logPath = FetchLog
	}
	tail, _ := sess.Run(ctx, "tail -n 20 "+shellQuote(logPath)+" 2>/dev/null")
	text := string(tail)
	switch {
	case strings.Contains(text, "FAILED "):
		return false, errs.Newf(errs.ClassHostFailure, "comfy.Download",
			"model fetch failed: %s", lastMatching(text, "FAILED "))
	case strings.Contains(text, "SHORT "):
		return false, errs.Newf(errs.ClassHostFailure, "comfy.Download",
			"model fetch truncated: %s", lastMatching(text, "SHORT "))
	}
	return false, nil
}

// BytesOnDisk totals what has landed, for progress reporting.
//
// Bytes on disk against bytes expected is the measure that answers the
// operator's real question. A rate alone says something is moving; it does not
// say whether that is two minutes from done or forty, and that difference
// decides whether to wait or to destroy.
func (d Download) BytesOnDisk(ctx context.Context, sess runtime.Session) (uint64, error) {
	out, err := sess.Run(ctx, "du -sb "+shellQuote(d.Dir)+" 2>/dev/null | cut -f1")
	if err != nil {
		return 0, err
	}
	var n uint64
	if _, err := fmt.Sscan(strings.TrimSpace(string(out)), &n); err != nil {
		return 0, errs.Newf(errs.ClassHostFailure, "comfy.Download",
			"unreadable disk usage")
	}
	return n, nil
}

// TotalBytes is what the whole fetch should come to.
func (d Download) TotalBytes() uint64 {
	var n uint64
	for _, it := range d.Items {
		n += it.Bytes
	}
	return n
}

func lastMatching(text, prefix string) string {
	var found string
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, prefix) {
			found = strings.TrimSpace(line)
		}
	}
	return found
}
