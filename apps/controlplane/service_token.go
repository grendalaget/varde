// `varde-control-plane service-token create|list|revoke`: operator-managed
// machine-to-machine credentials (vst_…) for provisioning services and other
// automation that must call a narrow slice of the API without a user session.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/grendalaget/varde/go/ids"

	"github.com/grendalaget/varde/apps/controlplane/internal/enroll"
	"github.com/grendalaget/varde/apps/controlplane/internal/store"
)

func serviceTokenCmd(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: varde-control-plane service-token create|list|revoke")
		return 2
	}
	var err error
	switch args[0] {
	case "create":
		err = serviceTokenCreate(args[1:])
	case "list":
		err = serviceTokenList(args[1:])
	case "revoke":
		err = serviceTokenRevoke(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown service-token action %q\n", args[0])
		return 2
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func serviceTokenFlags() (*flag.FlagSet, *string) {
	fs := flag.NewFlagSet("service-token", flag.ExitOnError)
	db := fs.String("db", envOr("VARDE_DB", defaultDBURL()), "database URL")
	return fs, db
}

// serviceScopes is the allowlist for --scopes. '*' is explicit opt-in to
// every scope; anything else is validated so a typo cannot quietly grant
// more (or less) than the operator meant.
var serviceScopes = map[string]struct{}{
	"enroll": {}, "members": {}, "nodes": {}, "servers": {}, "server_addons": {}, "events": {}, "steam_ids": {}, "*": {},
}

func openServiceStore(dbURL string) (*store.Store, error) {
	if p := sqlitePath(dbURL); p != "" {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return nil, fmt.Errorf("create db dir: %w", err)
		}
	}
	return store.Open(dbURL, store.RealClock)
}

func serviceTokenCreate(args []string) error {
	fs, dbURL := serviceTokenFlags()
	name := fs.String("name", "", "token name (appears in the audit log)")
	scopes := fs.String("scopes", "", "comma-separated scopes (enroll,members,nodes,servers,server_addons,events,steam_ids; * for all)")
	groups := fs.String("groups", "", "comma-separated group ids the token may act in (empty = all groups)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var scopeList []string
	for _, s := range strings.Split(*scopes, ",") {
		if s = strings.TrimSpace(s); s != "" {
			scopeList = append(scopeList, s)
		}
	}
	if *name == "" || len(scopeList) == 0 {
		return fmt.Errorf("--name and --scopes are required")
	}
	for _, s := range scopeList {
		if _, ok := serviceScopes[s]; !ok {
			return fmt.Errorf("unknown scope %q (known: enroll, members, nodes, servers, server_addons, events, steam_ids, *)", s)
		}
	}
	groupsProvided := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "groups" {
			groupsProvided = true
		}
	})
	var groupList []string
	for _, g := range strings.Split(*groups, ",") {
		if g = strings.TrimSpace(g); g != "" {
			groupList = append(groupList, g)
		}
	}
	// An explicit --groups that parses to nothing would silently create an
	// unbound (all-groups) token — fail closed instead.
	if groupsProvided && len(groupList) == 0 {
		return fmt.Errorf("--groups given but no group ids parsed (omit --groups for a token valid in every group)")
	}
	secret, err := enroll.ServiceToken()
	if err != nil {
		return err
	}
	st, err := openServiceStore(*dbURL)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	scopeJSON, _ := json.Marshal(scopeList)
	var groupJSON *string
	if len(groupList) > 0 {
		b, _ := json.Marshal(groupList)
		s := string(b)
		groupJSON = &s
	}
	t := &store.ServiceToken{
		ID:           ids.Must("svt_"),
		Name:         *name,
		TokenHash:    enroll.Hash(secret),
		ScopesJSON:   string(scopeJSON),
		GroupIDsJSON: groupJSON,
		CreatedAt:    st.NowMs(),
	}
	if err := st.CreateServiceToken(context.Background(), t); err != nil {
		return err
	}
	// The secret is shown once here and stored only as a SHA-256 hash.
	fmt.Println(secret)
	fmt.Fprintf(os.Stderr, "created %s name=%q scopes=%s\n", t.ID, t.Name, *scopes)
	return nil
}

func serviceTokenList(args []string) error {
	fs, dbURL := serviceTokenFlags()
	if err := fs.Parse(args); err != nil {
		return err
	}
	st, err := openServiceStore(*dbURL)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	ts, err := st.ListServiceTokens(context.Background())
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "ID\tNAME\tSCOPES\tCREATED\tLAST USED\tREVOKED")
	for _, t := range ts {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			t.ID, t.Name, t.ScopesJSON,
			tsMs(t.CreatedAt), tsMsPtr(t.LastUsedAt), tsMsPtr(t.RevokedAt))
	}
	return w.Flush()
}

func serviceTokenRevoke(args []string) error {
	fs, dbURL := serviceTokenFlags()
	id := fs.String("id", "", "service token id (svt_…)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *id == "" {
		return fmt.Errorf("--id is required")
	}
	st, err := openServiceStore(*dbURL)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	return st.RevokeServiceToken(context.Background(), *id)
}

func tsMs(ms int64) string {
	return time.UnixMilli(ms).UTC().Format("2006-01-02 15:04")
}

func tsMsPtr(p *int64) string {
	if p == nil {
		return "-"
	}
	return tsMs(*p)
}
