// The list and verify subcommands: show what the store holds, and re-hash
// every artifact against its sha256 sidecar.
package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/JaydenCJ/depshelf/internal/store"
)

func runList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	storeDir := fs.String("store", "./depshelf-store", "store directory (plain files)")
	format := fs.String("format", "text", "output format: text or json")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 0 || (*format != "text" && *format != "json") {
		fs.Usage()
		return exitUsage
	}
	st, err := store.Open(*storeDir)
	if err != nil {
		return runtimeErr(stderr, err)
	}
	pkgs, err := st.List()
	if err != nil {
		return runtimeErr(stderr, err)
	}
	if *format == "json" {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if pkgs == nil {
			pkgs = []store.Package{}
		}
		enc.Encode(pkgs)
		return exitOK
	}
	tw := tabwriter.NewWriter(stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ECOSYSTEM\tPACKAGE\tMETADATA\tARTIFACTS\tBYTES")
	var artifacts int
	var bytes int64
	for _, p := range pkgs {
		meta := "yes"
		if !p.HasMetadata {
			meta = "no"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\n", p.Ecosystem, p.Name, meta, p.Artifacts, p.Bytes)
		artifacts += p.Artifacts
		bytes += p.Bytes
	}
	tw.Flush()
	fmt.Fprintf(stdout, "%s, %s, %s\n",
		plural(int64(len(pkgs)), "package"), plural(int64(artifacts), "artifact"), plural(bytes, "byte"))
	return exitOK
}

// plural renders "<n> <noun>[s]" so summaries never read "1 packages".
func plural(n int64, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func runVerify(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	storeDir := fs.String("store", "./depshelf-store", "store directory (plain files)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return exitUsage
	}
	st, err := store.Open(*storeDir)
	if err != nil {
		return runtimeErr(stderr, err)
	}
	results, err := st.VerifyAll()
	if err != nil {
		return runtimeErr(stderr, err)
	}
	var ok, corrupt, unverified int
	for _, r := range results {
		switch r.Status {
		case "ok":
			ok++
		case "corrupt":
			corrupt++
			fmt.Fprintf(stdout, "corrupt     %s/%s/%s (%s)\n", r.Ecosystem, r.Name, r.File, r.Detail)
		default:
			unverified++
			fmt.Fprintf(stdout, "unverified  %s/%s/%s (%s)\n", r.Ecosystem, r.Name, r.File, r.Detail)
		}
	}
	fmt.Fprintf(stdout, "verified %s: %d ok, %d corrupt, %d unverified\n",
		plural(int64(len(results)), "artifact"), ok, corrupt, unverified)
	if corrupt > 0 {
		return exitFailed
	}
	return exitOK
}
