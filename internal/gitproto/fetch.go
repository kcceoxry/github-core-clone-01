// Fetch: the v2 and v0 upload-pack commands.
//
// A v2 request is one pkt-line stream: `command=fetch` (or `command=ls-refs`),
// optional agent and object-format lines, a delimiter, then the arguments up
// to a flush. A v0 request is two sections split by a flush: wants, shallow
// and deepen lines first, then have lines and `done`.
//
// Both answers carry the pack in sideband band 1. The v2 answer names its
// sections; the acknowledgments section is sent only when the client sent
// haves without `done`, and then it carries a `ready` line and ends with a
// delimiter. When the client sent `done` (or no haves), there is no
// acknowledgments section: the shallow-info section, if any, and the
// packfile section follow directly. The v0 answer sends the shallow list
// first (when the client asked for a depth), then one ACK/NAK line, then
// the sideband pack. A fetch that has nothing new to send still sends a
// pack: an empty one, 32 bytes. No response carries a trailing 0002: the
// stateless transport rejects it ("unexpected response end packet").
package gitproto

import (
	"fmt"
	"strconv"
	"strings"

	"forge/internal/pack"
	"forge/internal/store"
)

// v2Request is a parsed protocol v2 command: its name and its argument
// lines (the text after the delimiter, without trailing newlines).
type v2Request struct {
	command string
	args    []string
}

// parseV2Request decodes a v2 request body into its command and arguments.
// The first data packet must name the command; a delimiter separates the
// capability lines from the arguments; a flush ends the request. Unknown
// capability lines are ignored.
func parseV2Request(body []byte) (*v2Request, error) {
	packets, err := DecodePktLines(body)
	if err != nil {
		return nil, err
	}
	req := &v2Request{}
	seenDelim := false
	for _, packet := range packets {
		switch packet.Kind {
		case PacketFlush:
			if req.command == "" {
				return nil, fmt.Errorf("gitproto: v2 request ended before naming a command")
			}
			return req, nil
		case PacketDelim:
			seenDelim = true
		case PacketData:
			line := strings.TrimSuffix(string(packet.Payload), "\n")
			if req.command == "" {
				if !strings.HasPrefix(line, "command=") {
					return nil, fmt.Errorf("gitproto: v2 request starts with %q, not a command", line)
				}
				req.command = strings.TrimPrefix(line, "command=")
				continue
			}
			if !seenDelim {
				continue
			}
			req.args = append(req.args, line)
		case PacketResponseEnd:
			// Not expected in a request; ignore it.
		}
	}
	return nil, fmt.Errorf("gitproto: v2 request has no terminating flush")
}

// fetchArgs holds a fetch command's parsed arguments, for either version.
type fetchArgs struct {
	wants         []store.OID
	haves         []store.OID
	shallow       []store.OID
	depth         int
	deepenSince   int64
	deepenNot     []string
	filterBlobs   bool
	filterTrees   bool
	filterUnknown string
	done          bool
}

// parseFetchArgs parses fetch argument lines into a fetchArgs. A line it
// does not recognise is ignored, so a newer client cannot break the server.
func parseFetchArgs(lines []string) (*fetchArgs, error) {
	args := &fetchArgs{}
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "want":
			if len(fields) < 2 {
				return nil, fmt.Errorf("gitproto: want line has no object id")
			}
			oid, err := store.OIDFromHex(fields[1])
			if err != nil {
				return nil, fmt.Errorf("gitproto: want line names a bad object id %q", fields[1])
			}
			args.wants = append(args.wants, oid)
		case "have":
			if len(fields) < 2 {
				continue
			}
			oid, err := store.OIDFromHex(fields[1])
			if err != nil {
				continue
			}
			args.haves = append(args.haves, oid)
		case "shallow":
			if len(fields) < 2 {
				continue
			}
			oid, err := store.OIDFromHex(fields[1])
			if err != nil {
				continue
			}
			args.shallow = append(args.shallow, oid)
		case "deepen":
			if len(fields) < 2 {
				continue
			}
			depth, err := strconv.Atoi(fields[1])
			if err != nil || depth < 0 {
				continue
			}
			args.depth = depth
		case "deepen-since":
			if len(fields) < 2 {
				continue
			}
			since, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil {
				continue
			}
			args.deepenSince = since
		case "deepen-not":
			if len(fields) < 2 {
				continue
			}
			args.deepenNot = append(args.deepenNot, fields[1])
		case "filter":
			if len(fields) < 2 {
				continue
			}
			switch fields[1] {
			case "blob:none":
				args.filterBlobs = true
			case "tree:0":
				args.filterTrees = true
			default:
				args.filterUnknown = fields[1]
			}
		case "done":
			args.done = true
		}
	}
	return args, nil
}

// planAndPack runs the fetch planner for parsed arguments and builds the
// pack to send. A missing want is an error. Identical fetches share one
// cached plan (see packcache.go): the first builds it, concurrent ones
// wait for it, and later ones serve the cached pack bytes.
func planAndPack(st *store.Store, args *fetchArgs) (*FetchPlan, []byte, error) {
	opts := FetchOptions{
		Depth:       args.depth,
		Since:       args.deepenSince,
		NotRefs:     args.deepenNot,
		FilterBlobs: args.filterBlobs,
		FilterTrees: args.filterTrees,
	}
	key := fetchFingerprint(args.wants, args.haves, opts)
	if cached, ok := fetchPackCache.lookup(key); ok {
		// Objects is nil on a cache hit: the callers only read the
		// shallow lists, and the objects already live in the pack.
		return &FetchPlan{Shallow: cached.shallow, Unshallow: cached.unshallow}, cached.pack, nil
	}
	call, builder := fetchPackCache.begin(key)
	if !builder {
		<-call.done
		if cached, ok := fetchPackCache.lookup(key); ok {
			return &FetchPlan{Shallow: cached.shallow, Unshallow: cached.unshallow}, cached.pack, nil
		}
		// The first build failed; build it here instead of failing the
		// fetch for a transient error.
		return buildPlanAndPack(st, args, opts)
	}
	plan, packBytes, err := buildPlanAndPack(st, args, opts)
	if err != nil {
		fetchPackCache.fail(key, call)
		return nil, nil, err
	}
	fetchPackCache.finish(key, &cachedPlan{pack: packBytes, shallow: plan.Shallow, unshallow: plan.Unshallow}, call)
	return plan, packBytes, nil
}

// buildPlanAndPack plans one fetch and writes its pack, with no caching.
func buildPlanAndPack(st *store.Store, args *fetchArgs, opts FetchOptions) (*FetchPlan, []byte, error) {
	plan, err := PlanFetch(st, args.wants, args.haves, args.shallow, opts)
	if err != nil {
		return nil, nil, err
	}
	packBytes, err := pack.Write(plan.Objects)
	if err != nil {
		return nil, nil, err
	}
	return plan, packBytes, nil
}

// commonAcks returns the ACK lines for a fetch: one per have the store
// holds. When none is held the answer is a single NAK.
func commonAcks(st *store.Store, haves []store.OID) (acks []string, nak bool) {
	for _, have := range haves {
		if st.Has(have) {
			acks = append(acks, fmt.Sprintf("ACK %s", have.Hex()))
		}
	}
	return acks, len(acks) == 0
}

// HandleFetchV2 answers a v2 upload-pack request body: either ls-refs or
// fetch, by the command it names.
func HandleFetchV2(st *store.Store, body []byte) ([]byte, error) {
	req, err := parseV2Request(body)
	if err != nil {
		return nil, err
	}
	switch req.command {
	case "ls-refs":
		return handleLsRefsV2(st, req.args)
	case "fetch":
		return handleFetchV2(st, req.args)
	default:
		return nil, fmt.Errorf("gitproto: unknown v2 command %q", req.command)
	}
}

// handleLsRefsV2 answers a v2 ls-refs command from its argument lines.
// peel, symrefs and unborn are flags: the client sends the line only when
// it wants the feature, so each starts false and is set by its own line.
func handleLsRefsV2(st *store.Store, args []string) ([]byte, error) {
	opts := LsRefsOptions{}
	for _, line := range args {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "peel":
			opts.Peel = true
		case "symrefs":
			opts.Symrefs = true
		case "unborn":
			opts.Unborn = true
		case "ref-prefix":
			if len(fields) > 1 {
				opts.Prefixes = append(opts.Prefixes, fields[1])
			}
		}
	}
	return LsRefs(st, opts)
}

// handleFetchV2 answers a v2 fetch command. When the client sent `done`
// (or sent no haves), the response carries no acknowledgments section:
// the shallow-info section, if any, and the packfile section follow
// directly. Only when the client sent haves without `done` does the server
// answer acknowledgments, with a `ready` line and a delimiter, and the
// packfile in the same response. Real git goes straight to the packfile
// section when `done` was sent and dies ("expected 'packfile', received
// 'acknowledgments'") if an acknowledgments section is sent instead.
func handleFetchV2(st *store.Store, args []string) ([]byte, error) {
	fetch, err := parseFetchArgs(args)
	if err != nil {
		return nil, err
	}
	if len(fetch.wants) == 0 {
		return nil, fmt.Errorf("gitproto: fetch names no want")
	}
	plan, packBytes, err := planAndPack(st, fetch)
	if err != nil {
		return nil, err
	}

	var items []any
	if !fetch.done && len(fetch.haves) > 0 {
		acks, nak := commonAcks(st, fetch.haves)
		items = append(items, "acknowledgments\n")
		if nak {
			items = append(items, "NAK\n")
		} else {
			for _, ack := range acks {
				items = append(items, ack+"\n")
			}
		}
		items = append(items, "ready\n", "delim")
	}
	if len(plan.Shallow) > 0 || len(plan.Unshallow) > 0 {
		items = append(items, "shallow-info\n")
		for _, oid := range plan.Shallow {
			items = append(items, fmt.Sprintf("shallow %s\n", oid.Hex()))
		}
		for _, oid := range plan.Unshallow {
			items = append(items, fmt.Sprintf("unshallow %s\n", oid.Hex()))
		}
		items = append(items, "delim")
	}
	items = append(items, "packfile\n")
	chunks, err := SidebandChunks(packBytes)
	if err != nil {
		return nil, err
	}
	out, err := EncodePktLines(items)
	if err != nil {
		return nil, err
	}
	out = append(out, chunks...)
	out = append(out, []byte("0000")...)
	return out, nil
}

// HandleFetchV0 answers a v0 upload-pack request body: two sections split
// by a flush, wants first, then haves and done.
func HandleFetchV0(st *store.Store, body []byte) ([]byte, error) {
	packets, err := DecodePktLines(body)
	if err != nil {
		return nil, err
	}
	var first, second []string
	section := 0
	for _, packet := range packets {
		switch packet.Kind {
		case PacketFlush:
			section++
		case PacketData:
			line := strings.TrimSuffix(string(packet.Payload), "\n")
			if section == 0 {
				first = append(first, line)
			} else if section == 1 {
				second = append(second, line)
			}
		}
	}
	if section == 0 {
		return nil, fmt.Errorf("gitproto: v0 fetch has no section separator")
	}
	fetch, err := parseFetchArgs(append(first, second...))
	if err != nil {
		return nil, err
	}
	if len(fetch.wants) == 0 {
		return nil, fmt.Errorf("gitproto: fetch names no want")
	}
	plan, packBytes, err := planAndPack(st, fetch)
	if err != nil {
		return nil, err
	}

	// The v0 answer sends the shallow list first, then one ACK/NAK line,
	// then the sideband pack. The shallow list is a section of its own:
	// when the client asked for a depth it reads shallow/unshallow lines
	// until a flush, so the list is followed by a flush. Without a depth
	// request no shallow section is sent at all, and the client reads the
	// ack line directly. Only one ack line is sent: real git answers a
	// single `ACK <last common>` to a stateless request, and the client
	// stops reading acks after the first plain ACK.
	var items []any
	depthAsked := fetch.depth > 0 || fetch.deepenSince != 0 || len(fetch.deepenNot) > 0
	if (depthAsked || len(fetch.shallow) > 0) && (len(plan.Shallow) > 0 || len(plan.Unshallow) > 0) {
		for _, oid := range plan.Shallow {
			items = append(items, fmt.Sprintf("shallow %s\n", oid.Hex()))
		}
		for _, oid := range plan.Unshallow {
			items = append(items, fmt.Sprintf("unshallow %s\n", oid.Hex()))
		}
		items = append(items, "flush")
	}
	acks, nak := commonAcks(st, fetch.haves)
	if nak {
		items = append(items, "NAK\n")
	} else {
		items = append(items, acks[len(acks)-1]+"\n")
	}
	out, err := EncodePktLines(items)
	if err != nil {
		return nil, err
	}
	chunks, err := SidebandChunks(packBytes)
	if err != nil {
		return nil, err
	}
	out = append(out, chunks...)
	out = append(out, []byte("0000")...)
	return out, nil
}
