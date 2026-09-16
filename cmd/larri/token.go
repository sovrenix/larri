// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"errors"
	"flag"
	"fmt"
	"strings"

	"go.sovrenix.com/larri/internal/clientkeys"
	"go.sovrenix.com/larri/internal/daemon"
	"go.sovrenix.com/larri/internal/secret"
)

const tokenUsage = `larri token — API keys for the local /v1 endpoint

  larri token create <name>   make a key for one client; it is shown once
  larri token list            the keys that exist, by name
  larri token revoke <name>   stop accepting a key, including on a rig serving now

A key is accepted by every rig, so a client is configured once. Only a hash is
stored: a key that is lost is revoked and replaced, never shown again.
`

func openClientKeys() *clientkeys.Store { return clientkeys.Open(stateDir()) }

// cmdToken manages the stored client keys.
func cmdToken(args []string) error {
	fs := flag.NewFlagSet("token", flag.ExitOnError)
	fs.Usage = func() { fmt.Print(tokenUsage) }
	_ = fs.Parse(args)
	keys := openClientKeys()
	switch fs.Arg(0) {
	case "create":
		name := fs.Arg(1)
		if name == "" {
			return errors.New("token create: a name is required: larri token create <name>")
		}
		key, err := keys.Create(name)
		if err != nil {
			return err
		}
		fmt.Printf("  %s\n\n", key.Reveal())
		fmt.Printf("  client key %q — shown once, now. Put it in the client's config; LARRI keeps only a hash.\n", name)
		return nil
	case "list":
		list, err := keys.List()
		if err != nil {
			return err
		}
		if len(list) == 0 {
			fmt.Println("  no client keys — the next larri up creates one called default")
			return nil
		}
		for _, k := range list {
			fmt.Printf("  %-32s created %s\n", k.Name, k.Created.Format("2006-01-02 15:04 MST"))
		}
		return nil
	case "revoke":
		name := fs.Arg(1)
		if name == "" {
			return errors.New("token revoke: a name is required: larri token revoke <name>")
		}
		if err := keys.Revoke(name); err != nil {
			return err
		}
		fmt.Printf("  client key %q revoked — a rig serving now stops accepting it on the next request\n", name)
		return nil
	case "", "help":
		fmt.Print(tokenUsage)
		return nil
	}
	return fmt.Errorf("token: unknown subcommand %q: expected create, list or revoke", fs.Arg(0))
}

// keyLine says, once a rig is serving, which key a client uses.
//
// A key for this rig alone is shown. Otherwise the stored keys are named — they
// were shown when they were made — and on a first run, with none stored, one
// called default is made now and shown, once. It is made here rather than
// before the bring-up so a run that never reaches READY leaves no key nobody
// saw; the proxy picks it up from the file on the next request.
func keyLine(live rigKeys, keys *clientkeys.Store) (string, error) {
	_, line, err := keyInfo(live, keys)
	return line, err
}

// rigKeys is what keyInfo needs of a serving rig; *daemon.Live has it.
type rigKeys interface {
	RigKey() secret.Secret
	AddRigKey() (secret.Secret, error)
}

var _ rigKeys = (*daemon.Live)(nil)

// keyInfo is keyLine with the key's value apart, when one is being shown, for
// a caller that reports it as data rather than as a sentence.
//
// A rig is READY and billing by the time this runs, so a key store that cannot
// be read, or a default key that cannot be written, does not leave it with no
// usable key: the rig is given one of its own, and the line says why.
func keyInfo(live rigKeys, keys *clientkeys.Store) (value, line string, err error) {
	if k := live.RigKey(); !k.Empty() {
		v := k.Reveal()
		return v, fmt.Sprintf("%s   (this rig only — shown once)", v), nil
	}
	list, err := keys.List()
	if err == nil && len(list) == 0 {
		var key secret.Secret
		if key, err = keys.Create("default"); err == nil {
			v := key.Reveal()
			return v, fmt.Sprintf("%s   (client key \"default\", created now — shown once; larri token list)", v), nil
		}
	}
	if err != nil {
		k, rerr := live.AddRigKey()
		if rerr != nil {
			return "", "", fmt.Errorf("%w; and no key of its own: %v", err, rerr)
		}
		v := k.Reveal()
		return v, fmt.Sprintf("%s   (this rig only — shown once; stored client keys unusable: %v)", v, err), nil
	}
	names := make([]string, len(list))
	for i, k := range list {
		names[i] = k.Name
	}
	return "", fmt.Sprintf("your client keys: %s   (larri token create <name> adds one)", strings.Join(names, ", ")), nil
}
