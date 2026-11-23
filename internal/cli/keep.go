package cli

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/itswl/agent-session-query/internal/app"
	"github.com/itswl/agent-session-query/internal/catalog"
)

func cmdKeep(ctx context.Context, args []string, io *streams) error {
	c := newCommand("keep", io.err)
	all := c.fs.Bool("all", false, "keep a copy of every session of the CLIs that delete theirs, as each changes")
	off := c.fs.Bool("off", false, "stop keeping every session; what is kept stays, and so do the sessions kept one by one")
	drop := c.fs.Bool("drop", false, "stop keeping the session named one by one")
	dir := c.fs.String("dir", "", "the folder the copies go in (default: kept/ in asq's state directory)")
	rest, err := c.parse(args)
	if err != nil {
		return err
	}
	switch {
	case *all && *off:
		return usagef("--all or --off, not both")
	case len(rest) > 1, *drop && len(rest) == 0:
		return usagef("asq keep [--all | --off] [--dir folder] | asq keep [--drop] <ref>")
	case len(rest) == 1 && (*all || *off):
		return usagef("--all and --off are about every session; a ref names one")
	}
	a, err := open(ctx, c, io)
	if err != nil {
		return err
	}
	defer a.Close()
	if *dir != "" {
		if err := a.KeepIn(*dir); err != nil {
			return err
		}
	}

	if len(rest) == 1 {
		s, _, err := resolve(ctx, a, rest[0])
		if err != nil {
			return err
		}
		k, err := a.KeepSession(ctx, &s, !*drop)
		if err != nil {
			return fmt.Errorf("%s: %w", s.Ref, err)
		}
		if c.json {
			return printJSON(io.out, k)
		}
		ks, _ := a.KeepStatus(ctx)
		switch {
		case !*drop:
			with := ""
			if s.Agents > 0 {
				with = ", and its subagents with it"
			}
			fmt.Fprintf(io.out, "kept %s%s, in %s: %s copied now. It is copied again a few minutes after it changes.\n", s.Ref, with, ks.Folder, size(k.Bytes))
		case k.Stays == "all":
			spec, _ := catalog.Spec(s.Source)
			fmt.Fprintf(io.out, "%s is no longer kept one by one, and is still kept: asq keeps every session of %s (asq keep --off stops that).\n", s.Ref, spec.Title)
		case k.Stays == "gone":
			fmt.Fprintf(io.out, "%s is no longer kept one by one. Its CLI has deleted it, so its copy is all there is of it, and stays in %s: delete it there to be rid of it.\n", s.Ref, ks.Folder)
		default:
			fmt.Fprintf(io.out, "%s is no longer kept, and its copy is deleted: its CLI still holds it.\n", s.Ref)
		}
		return nil
	}

	var copied app.Copied
	if *all || *off {
		if copied, err = a.KeepAll(ctx, *all); err != nil {
			return err
		}
	}
	ks, err := a.KeepStatus(ctx)
	if err != nil {
		return err
	}
	if c.json {
		return printJSON(io.out, ks)
	}
	of := strings.Join(ks.Of, " and ")
	switch {
	case ks.All && *all:
		fmt.Fprintf(io.out, "asq keeps a copy of every session of %s from now on, as each changes: %d copied now, %s.\n", of, copied.Transcripts, size(copied.Bytes))
	case ks.All:
		fmt.Fprintf(io.out, "asq keeps a copy of every session of %s, which delete theirs, as each changes.\n", of)
	case len(ks.Sessions) > 0:
		fmt.Fprintf(io.out, "asq keeps a copy of %s, as each changes (asq keep --all keeps every session of %s).\n", plural(len(ks.Sessions), "session"), of)
	case ks.Files > 0:
		fmt.Fprintf(io.out, "asq keeps nothing more: what it kept stays (asq keep --all keeps every session of %s).\n", of)
	default:
		fmt.Fprintf(io.out, "asq keeps no copies. %s delete the sessions not used for 30 days by default, and they leave asq too: asq keep --all keeps a copy of each, as it changes, and asq keep <ref> one session.\n", of)
		return nil
	}
	if ks.All && len(ks.Sessions) > 0 {
		fmt.Fprintf(io.out, "  and of %s kept one by one\n", plural(len(ks.Sessions), "session"))
	}
	fmt.Fprintf(io.out, "  in %s: %s, %s\n", ks.Folder, plural(ks.Files, "file"), size(ks.Bytes))
	for _, s := range ks.Stores {
		gone := ""
		if s.Gone > 0 {
			gone = fmt.Sprintf(", %d of them deleted by %s since", s.Gone, s.Title)
		}
		fmt.Fprintf(io.out, "  %-12s %s%s\n", s.Source, plural(s.Transcripts, "transcript"), gone)
	}
	if ks.Error != "" {
		fmt.Fprintf(io.out, "  ! the last copy did not go through whole: %s\n", ks.Error)
	}
	return nil
}

// size is a size on disk, for a person to read.
func size(n int64) string {
	switch {
	case n < 1<<10:
		return plural(int(n), "byte")
	case n < 1<<20:
		return strconv.FormatInt((n+1<<9)>>10, 10) + " KB"
	case n < 1<<30:
		return strconv.FormatFloat(float64(n)/(1<<20), 'f', 1, 64) + " MB"
	}
	return strconv.FormatFloat(float64(n)/(1<<30), 'f', 1, 64) + " GB"
}
