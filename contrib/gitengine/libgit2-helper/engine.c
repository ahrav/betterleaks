#include <errno.h>
#include <git2.h>
#include <inttypes.h>
#include <stdbool.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <strings.h>

#define PROTOCOL_VERSION 1u
#define MAX_FRAME (16u << 20)
#define MAX_RECORD (256u << 20)
#define FRAME_HEADER 12u
#define OBJECT_CACHE_LIMIT (128u << 20)
#define MAP_WINDOW_SIZE (64u << 20)
#define MAPPED_LIMIT (128u << 20)

/* The stock-Git contract needs two compatibility layers that public libgit2
 * does not enable itself: core Git's zero-context common-tail reduction and
 * its chunk-count rename similarity. Define BL_NATIVE_LIBGIT2_SEMANTICS only
 * for controlled falsification builds. */
#ifndef BL_NATIVE_LIBGIT2_SEMANTICS
#ifndef BL_PUBLIC_TRIM_DIFF
#define BL_PUBLIC_TRIM_DIFF 1
#endif
#ifndef BL_CANONICAL_SIMILARITY
#define BL_CANONICAL_SIMILARITY 1
#endif
#endif

#ifndef BL_DIFF_FLAGS
#define BL_DIFF_FLAGS GIT_DIFF_INDENT_HEURISTIC
#endif

#ifndef BL_INTERHUNK_LINES
#define BL_INTERHUNK_LINES 0u
#endif

#ifndef BL_FIND_FLAGS
#define BL_FIND_FLAGS GIT_DIFF_FIND_RENAMES
#endif

#ifndef BL_RENAME_LIMIT
#define BL_RENAME_LIMIT 1000u
#endif

#ifndef BL_RENAME_THRESHOLD
#define BL_RENAME_THRESHOLD 50u
#endif

typedef struct {
	unsigned char *data;
	size_t len;
	size_t cap;
} buffer;

typedef struct {
	char tag[4];
	uint64_t batch;
	buffer payload;
} frame;

typedef struct {
	git_repository *repo;
	git_odb *odb;
	git_mailmap *mailmap;
	size_t oid_len;
	bool all_statuses;
	bool find_copies;
} engine;

typedef struct {
	engine *eng;
	uint64_t batch;
	const git_oid *commit_oid;
	uint64_t files;
	uint64_t hunks;
	bool file_open;
	bool hunk_open;
	buffer hunk_added;
	uint64_t hunk_position;
	bool hunk_missing_nl;
	const char *new_path;
} diff_sink;

#ifdef BL_CANONICAL_SIMILARITY
typedef struct {
	uint64_t hash;
	uint32_t len;
} similarity_span;

typedef struct {
	similarity_span *spans;
	size_t count;
	size_t total;
} similarity_signature;

static int similarity_span_cmp(const void *left, const void *right)
{
	const similarity_span *a = left;
	const similarity_span *b = right;
	if (a->hash != b->hash)
		return a->hash < b->hash ? -1 : 1;
	return 0;
}

static int make_similarity_signature(void **out, const void *raw, size_t raw_len)
{
	const unsigned char *data = raw;
	bool text = true;
	for (size_t i = 0; i < raw_len && i < 8000; i++) {
		if (data[i] == 0) {
			text = false;
			break;
		}
	}
	similarity_signature *signature = calloc(1, sizeof(*signature));
	if (!signature)
		return -1;
	size_t capacity = raw_len / 32 + 1;
	signature->spans = calloc(capacity, sizeof(*signature->spans));
	if (!signature->spans) {
		free(signature);
		return -1;
	}
	uint32_t accum1 = 0, accum2 = 0;
	uint32_t length = 0;
	for (size_t i = 0; i < raw_len; i++) {
		unsigned char value = data[i];
		if (text && value == '\r' && i + 1 < raw_len && data[i + 1] == '\n')
			continue;
		uint32_t previous = accum1;
		accum1 = (accum1 << 7) ^ (accum2 >> 25);
		accum2 = (accum2 << 7) ^ (previous >> 25);
		accum1 += value;
		length++;
		if (length < 64 && value != '\n')
			continue;
		if (signature->count == capacity) {
			capacity *= 2;
			void *grown = realloc(signature->spans, capacity * sizeof(*signature->spans));
			if (!grown) {
				free(signature->spans);
				free(signature);
				return -1;
			}
			signature->spans = grown;
		}
		uint64_t hash = (accum1 + accum2 * UINT32_C(0x61)) % UINT32_C(107927);
		signature->spans[signature->count++] = (similarity_span){hash, length};
		signature->total += length;
		accum1 = accum2 = 0;
		length = 0;
	}
	if (length) {
		if (signature->count == capacity) {
			capacity *= 2;
			void *grown = realloc(signature->spans, capacity * sizeof(*signature->spans));
			if (!grown) {
				free(signature->spans);
				free(signature);
				return -1;
			}
			signature->spans = grown;
		}
		uint64_t hash = (accum1 + accum2 * UINT32_C(0x61)) % UINT32_C(107927);
		signature->spans[signature->count++] = (similarity_span){hash, length};
		signature->total += length;
	}
	qsort(signature->spans, signature->count, sizeof(*signature->spans),
	      similarity_span_cmp);
	*out = signature;
	return 0;
}

static int canonical_file_signature(void **out, const git_diff_file *file,
				    const char *fullpath, void *payload)
{
	(void)fullpath;
	engine *e = payload;
	git_blob *blob = NULL;
	if (git_blob_lookup(&blob, e->repo, &file->id) < 0) {
		if (file->mode == GIT_FILEMODE_COMMIT)
			return make_similarity_signature(out, file->id.id, e->oid_len);
		return -1;
	}
	int rc = make_similarity_signature(out, git_blob_rawcontent(blob),
					   (size_t)git_blob_rawsize(blob));
	git_blob_free(blob);
	return rc;
}

static int canonical_buffer_signature(void **out, const git_diff_file *file,
				      const char *buf, size_t buflen, void *payload)
{
	(void)file;
	(void)payload;
	return make_similarity_signature(out, buf, buflen);
}

static void canonical_free_signature(void *raw, void *payload)
{
	(void)payload;
	similarity_signature *signature = raw;
	if (signature) {
		free(signature->spans);
		free(signature);
	}
}

static int canonical_similarity(int *score, void *left, void *right, void *payload)
{
	(void)payload;
	similarity_signature *a = left;
	similarity_signature *b = right;
	size_t i = 0, j = 0, copied = 0;
	while (i < a->count && j < b->count) {
		int order = similarity_span_cmp(&a->spans[i], &b->spans[j]);
		if (order < 0) {
			i++;
			continue;
		}
		if (order > 0) {
			j++;
			continue;
		}
		size_t ai = i + 1, bj = j + 1;
		size_t a_bytes = a->spans[i].len, b_bytes = b->spans[j].len;
		while (ai < a->count && similarity_span_cmp(&a->spans[i], &a->spans[ai]) == 0)
			a_bytes += a->spans[ai++].len;
		while (bj < b->count && similarity_span_cmp(&b->spans[j], &b->spans[bj]) == 0)
			b_bytes += b->spans[bj++].len;
		copied += a_bytes < b_bytes ? a_bytes : b_bytes;
		i = ai;
		j = bj;
	}
	size_t maximum = a->total > b->total ? a->total : b->total;
	*score = maximum ? (int)(copied * 100 / maximum) : 0;
	return 0;
}
#endif

static uint32_t be32(const unsigned char *p)
{
	return ((uint32_t)p[0] << 24) | ((uint32_t)p[1] << 16) |
	       ((uint32_t)p[2] << 8) | (uint32_t)p[3];
}

static uint64_t be64(const unsigned char *p)
{
	return ((uint64_t)be32(p) << 32) | be32(p + 4);
}

static void put16(unsigned char *p, uint16_t v)
{
	p[0] = (unsigned char)(v >> 8);
	p[1] = (unsigned char)v;
}

static void put32(unsigned char *p, uint32_t v)
{
	p[0] = (unsigned char)(v >> 24);
	p[1] = (unsigned char)(v >> 16);
	p[2] = (unsigned char)(v >> 8);
	p[3] = (unsigned char)v;
}

static void put64(unsigned char *p, uint64_t v)
{
	put32(p, (uint32_t)(v >> 32));
	put32(p + 4, (uint32_t)v);
}

static void buffer_dispose(buffer *b)
{
	free(b->data);
	memset(b, 0, sizeof(*b));
}

static int buffer_reserve(buffer *b, size_t extra)
{
	if (extra > SIZE_MAX - b->len)
		return -1;
	size_t need = b->len + extra;
	if (need <= b->cap)
		return 0;
	size_t cap = b->cap ? b->cap : 256;
	while (cap < need) {
		if (cap > SIZE_MAX / 2) {
			cap = need;
			break;
		}
		cap *= 2;
	}
	void *p = realloc(b->data, cap);
	if (!p)
		return -1;
	b->data = p;
	b->cap = cap;
	return 0;
}

static int buffer_add(buffer *b, const void *data, size_t len)
{
	if (buffer_reserve(b, len) < 0)
		return -1;
	if (len)
		memcpy(b->data + b->len, data, len);
	b->len += len;
	return 0;
}

static int buffer_u8(buffer *b, uint8_t v) { return buffer_add(b, &v, 1); }

static int buffer_u32(buffer *b, uint32_t v)
{
	unsigned char p[4];
	put32(p, v);
	return buffer_add(b, p, sizeof(p));
}

static int buffer_u64(buffer *b, uint64_t v)
{
	unsigned char p[8];
	put64(p, v);
	return buffer_add(b, p, sizeof(p));
}

static int buffer_bytes(buffer *b, const void *data, size_t len)
{
	if (len > UINT32_MAX || buffer_u32(b, (uint32_t)len) < 0)
		return -1;
	return buffer_add(b, data, len);
}

static int write_all(FILE *out, const void *data, size_t len)
{
	const unsigned char *p = data;
	while (len) {
		size_t n = fwrite(p, 1, len, out);
		if (!n)
			return -1;
		p += n;
		len -= n;
	}
	return 0;
}

static int write_frame(const char tag[4], uint64_t batch, const void *payload,
		       size_t payload_len)
{
	if (payload_len > MAX_FRAME - FRAME_HEADER)
		return -1;
	unsigned char hdr[16];
	put32(hdr, (uint32_t)(FRAME_HEADER + payload_len));
	memcpy(hdr + 4, tag, 4);
	put64(hdr + 8, batch);
	if (write_all(stdout, hdr, sizeof(hdr)) < 0 ||
	    write_all(stdout, payload, payload_len) < 0 || fflush(stdout) != 0)
		return -1;
	return 0;
}

static int write_empty(const char tag[4], uint64_t batch)
{
	return write_frame(tag, batch, NULL, 0);
}

static const char *last_git_error(void)
{
	const git_error *error = git_error_last();
	return error && error->message ? error->message : "unknown libgit2 error";
}

static int write_error(uint64_t batch, uint8_t kind, const char *op,
		       const char *message)
{
	buffer b = {0};
	int rc = buffer_u8(&b, kind);
	if (!rc)
		rc = buffer_bytes(&b, op, strlen(op));
	if (!rc)
		rc = buffer_bytes(&b, message, strlen(message));
	if (!rc)
		rc = write_frame("ERRO", batch, b.data, b.len);
	buffer_dispose(&b);
	return rc;
}

static int error_and_fail(uint64_t batch, uint8_t kind, const char *op,
			  const char *message)
{
	(void)write_error(batch, kind, op, message);
	return -1;
}

static int read_all(FILE *in, void *data, size_t len)
{
	unsigned char *p = data;
	while (len) {
		size_t n = fread(p, 1, len, in);
		if (!n)
			return feof(in) ? 1 : -1;
		p += n;
		len -= n;
	}
	return 0;
}

static int read_frame(frame *f)
{
	unsigned char prefix[4];
	int rc = read_all(stdin, prefix, sizeof(prefix));
	if (rc)
		return rc;
	uint32_t len = be32(prefix);
	if (len < FRAME_HEADER || len > MAX_FRAME)
		return -2;
	unsigned char header[FRAME_HEADER];
	if (read_all(stdin, header, sizeof(header)))
		return -2;
	memcpy(f->tag, header, 4);
	f->batch = be64(header + 4);
	size_t payload_len = len - FRAME_HEADER;
	if (buffer_reserve(&f->payload, payload_len) < 0)
		return -3;
	f->payload.len = payload_len;
	if (read_all(stdin, f->payload.data, payload_len))
		return -2;
	return 0;
}

static int textconv_cb(const git_config_entry *entry, void *payload)
{
	(void)entry;
	*(bool *)payload = true;
	return 1;
}

static int isolate_external_config(void)
{
	/*
	 * The engine contract is repository-scoped.  An operator's config must not
	 * change whether a repository can use this helper, nor may shared attribute
	 * files change record semantics.  An empty search path disables each
	 * non-repository level while leaving LOCAL and WORKTREE configuration, plus
	 * in-repository attributes, available through the repository object.
	 */
	const git_config_level_t levels[] = {
		GIT_CONFIG_LEVEL_PROGRAMDATA,
		GIT_CONFIG_LEVEL_SYSTEM,
		GIT_CONFIG_LEVEL_XDG,
		GIT_CONFIG_LEVEL_GLOBAL,
	};
	for (size_t i = 0; i < sizeof(levels) / sizeof(levels[0]); i++) {
		if (git_libgit2_opts(GIT_OPT_SET_SEARCH_PATH, levels[i], "") < 0)
			return -1;
	}
	return 0;
}

static int preflight_config(git_repository *repo, char *reason, size_t reason_len)
{
	git_config *cfg = NULL;
	bool textconv = false;
	if (git_repository_config_snapshot(&cfg, repo) < 0) {
		snprintf(reason, reason_len, "config snapshot: %s", last_git_error());
		return -1;
	}
	int rc = git_config_foreach_match(cfg, "^diff\\..*\\.textconv$", textconv_cb,
					  &textconv);
	if (rc < 0 && !textconv) {
		snprintf(reason, reason_len, "config scan: %s", last_git_error());
		git_config_free(cfg);
		return -1;
	}
	const char *external = NULL;
	bool has_external = git_config_get_string(&external, cfg, "diff.external") == 0;
	const char *encoding = NULL;
	bool has_encoding = git_config_get_string(&encoding, cfg, "i18n.commitencoding") == 0;
	/* git_config_get_string() returns storage owned by cfg.  Evaluate the
	 * encoding while the snapshot is alive instead of retaining that pointer
	 * across git_config_free(). */
	bool unsupported_encoding = has_encoding && encoding &&
		strcasecmp(encoding, "UTF-8") != 0 &&
		strcasecmp(encoding, "UTF8") != 0;
	git_config_free(cfg);
	if (textconv) {
		snprintf(reason, reason_len, "unsupported diff.*.textconv configuration");
		return 1;
	}
	if (has_external || getenv("GIT_EXTERNAL_DIFF")) {
		snprintf(reason, reason_len, "unsupported external diff configuration");
		return 1;
	}
	if (unsupported_encoding) {
		snprintf(reason, reason_len, "unsupported commit encoding configuration");
		return 1;
	}
	return 0;
}

static int write_hello(const engine *e)
{
	unsigned char p[6];
	put16(p, PROTOCOL_VERSION);
	put16(p + 2, (uint16_t)e->oid_len);
	p[4] = e->all_statuses;
	p[5] = e->find_copies;
	return write_frame("HELO", 0, p, sizeof(p));
}

static int decode_oid(git_oid *oid, const unsigned char *raw, size_t len,
		      size_t expected)
{
	if (len != expected)
		return -1;
#ifdef GIT_EXPERIMENTAL_SHA256
	git_oid_t type = expected == GIT_OID_SHA256_SIZE ? GIT_OID_SHA256 : GIT_OID_SHA1;
	return git_oid_fromraw(oid, raw, type);
#else
	if (expected != GIT_OID_SHA1_SIZE)
		return -1;
	return git_oid_fromraw(oid, raw);
#endif
}

static int add_oid_bytes(buffer *b, const git_oid *oid, size_t oid_len)
{
	return buffer_bytes(b, oid->id, oid_len);
}

static int add_zero_oid(buffer *b, size_t oid_len)
{
	unsigned char zero[GIT_OID_MAX_SIZE] = {0};
	return buffer_bytes(b, zero, oid_len);
}

static char status_char(git_delta_t status)
{
	switch (status) {
	case GIT_DELTA_ADDED: return 'A';
	case GIT_DELTA_COPIED: return 'C';
	case GIT_DELTA_DELETED: return 'D';
	case GIT_DELTA_MODIFIED: return 'M';
	case GIT_DELTA_RENAMED: return 'R';
	case GIT_DELTA_TYPECHANGE: return 'T';
	case GIT_DELTA_CONFLICTED: return 'U';
	case GIT_DELTA_UNREADABLE: return 'X';
	default: return 0;
	}
}

static bool allowed_status(const engine *e, char status)
{
	if (e->all_statuses)
		return status == 'A' || status == 'C' || status == 'D' || status == 'M' ||
		       status == 'R' || status == 'T' || status == 'U' || status == 'X';
	return status == 'A' || status == 'M' || status == 'R' ||
	       (e->find_copies && status == 'C');
}

static int write_hunk(diff_sink *sink)
{
	if (!sink->hunk_open)
		return 0;
	buffer meta = {0};
	int rc = add_oid_bytes(&meta, sink->commit_oid, sink->eng->oid_len);
	if (!rc)
		rc = buffer_bytes(&meta, sink->new_path, strlen(sink->new_path));
	if (!rc)
		rc = buffer_u64(&meta, sink->hunk_position);
	if (rc)
		goto done;
	size_t regular_len = meta.len + 4 + sink->hunk_added.len + 1;
	if (regular_len <= MAX_FRAME - FRAME_HEADER) {
		rc = buffer_bytes(&meta, sink->hunk_added.data, sink->hunk_added.len);
		if (!rc)
			rc = buffer_u8(&meta, sink->hunk_missing_nl);
		if (!rc)
			rc = write_frame("HUNK", sink->batch, meta.data, meta.len);
	} else {
		rc = write_frame("HBGN", sink->batch, meta.data, meta.len);
		for (size_t off = 0; !rc && off < sink->hunk_added.len;) {
			size_t n = sink->hunk_added.len - off;
			if (n > MAX_FRAME - FRAME_HEADER)
				n = MAX_FRAME - FRAME_HEADER;
			rc = write_frame("HADD", sink->batch, sink->hunk_added.data + off, n);
			off += n;
		}
		unsigned char missing = sink->hunk_missing_nl;
		if (!rc)
			rc = write_frame("HEND", sink->batch, &missing, 1);
	}
	if (!rc)
		sink->hunks++;
done:
	buffer_dispose(&meta);
	sink->hunk_added.len = 0;
	sink->hunk_open = false;
	sink->hunk_missing_nl = false;
	return rc;
}

static int close_file(diff_sink *sink)
{
	if (write_hunk(sink) < 0)
		return -1;
	if (sink->file_open && write_empty("FEND", sink->batch) < 0)
		return -1;
	sink->file_open = false;
	return 0;
}

static int file_cb(const git_diff_delta *delta, float progress, void *payload)
{
	(void)progress;
	diff_sink *sink = payload;
	if (close_file(sink) < 0)
		return -1;
	char status = status_char(delta->status);
	if (!status || !allowed_status(sink->eng, status))
		return 0;
	const char *old_path = delta->old_file.path ? delta->old_file.path : "";
	const char *new_path = delta->new_file.path ? delta->new_file.path : "";
	buffer b = {0};
	bool old_exists = (delta->old_file.flags & GIT_DIFF_FLAG_EXISTS) != 0;
	bool new_exists = (delta->new_file.flags & GIT_DIFF_FLAG_EXISTS) != 0;
	// Core Git's patch stream emits no binary marker for an unchanged blob
	// (pure rename or mode-only change), even when the blob content is binary.
	// Binary is the parser-visible patch classification, not a blob property.
	bool binary = (delta->flags & GIT_DIFF_FLAG_BINARY) != 0 &&
		      !git_oid_equal(&delta->old_file.id, &delta->new_file.id);
	int rc = add_oid_bytes(&b, sink->commit_oid, sink->eng->oid_len);
	if (!rc)
		rc = buffer_u8(&b, (uint8_t)status);
	if (!rc)
		rc = buffer_u32(&b, delta->old_file.mode);
	if (!rc)
		rc = buffer_u32(&b, delta->new_file.mode);
	if (!rc)
		rc = old_exists ? add_oid_bytes(&b, &delta->old_file.id, sink->eng->oid_len)
				: add_zero_oid(&b, sink->eng->oid_len);
	if (!rc)
		rc = new_exists ? add_oid_bytes(&b, &delta->new_file.id, sink->eng->oid_len)
				: add_zero_oid(&b, sink->eng->oid_len);
	if (!rc)
		rc = buffer_bytes(&b, old_path, strlen(old_path));
	if (!rc)
		rc = buffer_bytes(&b, new_path, strlen(new_path));
	if (!rc)
		rc = buffer_u8(&b, binary);
	if (!rc)
		rc = write_frame("FBEG", sink->batch, b.data, b.len);
	buffer_dispose(&b);
	if (rc)
		return -1;
	sink->file_open = true;
	sink->new_path = new_path;
	sink->files++;
	return 0;
}

#ifndef BL_PUBLIC_TRIM_DIFF
static int binary_cb(const git_diff_delta *delta, const git_diff_binary *binary,
		     void *payload)
{
	(void)delta;
	(void)binary;
	(void)payload;
	return 0;
}
#endif

static int hunk_cb(const git_diff_delta *delta, const git_diff_hunk *hunk,
		   void *payload)
{
	(void)delta;
	diff_sink *sink = payload;
	if (!sink->file_open)
		return 0;
	if (write_hunk(sink) < 0)
		return -1;
	sink->hunk_open = true;
	sink->hunk_position = hunk->new_start < 0 ? 0 : (uint64_t)hunk->new_start;
	return 0;
}

static int line_cb(const git_diff_delta *delta, const git_diff_hunk *hunk,
		   const git_diff_line *line, void *payload)
{
	(void)delta;
	(void)hunk;
	diff_sink *sink = payload;
	if (!sink->file_open || !sink->hunk_open)
		return 0;
	if (line->origin == GIT_DIFF_LINE_ADDITION) {
		if (line->content_len > MAX_RECORD - sink->hunk_added.len)
			return -1;
		if (buffer_add(&sink->hunk_added, line->content, line->content_len) < 0)
			return -1;
	} else if (line->origin == GIT_DIFF_LINE_CONTEXT_EOFNL ||
		   line->origin == GIT_DIFF_LINE_DEL_EOFNL) {
		sink->hunk_missing_nl = true;
	}
	return 0;
}

#ifdef BL_PUBLIC_TRIM_DIFF
static int ignore_file_cb(const git_diff_delta *delta, float progress, void *payload)
{
	(void)delta;
	(void)progress;
	(void)payload;
	return 0;
}

static void trim_large_common_tail(const void *old_data, size_t *old_len,
				   const void *new_data, size_t *new_len)
{
	const size_t block = 1024;
	size_t trimmed = 0, retained = 0;
	size_t smaller = *old_len < *new_len ? *old_len : *new_len;
	if (smaller < block)
		return;
	const unsigned char *old_end = (const unsigned char *)old_data + *old_len;
	const unsigned char *new_end = (const unsigned char *)new_data + *new_len;
	while (block + trimmed <= smaller &&
	       memcmp(old_end - block, new_end - block, block) == 0) {
		trimmed += block;
		old_end -= block;
		new_end -= block;
	}
	while (retained < trimmed && old_end[retained++] != '\n')
		;
	*old_len -= trimmed - retained;
	*new_len -= trimmed - retained;
}

static int emit_buffer_hunks(diff_sink *sink, const git_diff_delta *delta,
			     const git_diff_options *base_options)
{
	git_blob *old_blob = NULL, *new_blob = NULL;
	const void *old_data = NULL, *new_data = NULL;
	size_t old_len = 0, new_len = 0;
	if (delta->old_file.flags & GIT_DIFF_FLAG_EXISTS) {
		if (git_blob_lookup(&old_blob, sink->eng->repo, &delta->old_file.id) < 0)
			return -1;
		old_data = git_blob_rawcontent(old_blob);
		old_len = (size_t)git_blob_rawsize(old_blob);
	}
	if (delta->new_file.flags & GIT_DIFF_FLAG_EXISTS) {
		if (git_blob_lookup(&new_blob, sink->eng->repo, &delta->new_file.id) < 0) {
			git_blob_free(old_blob);
			return -1;
		}
		new_data = git_blob_rawcontent(new_blob);
		new_len = (size_t)git_blob_rawsize(new_blob);
	}
	trim_large_common_tail(old_data, &old_len, new_data, &new_len);
	git_diff_options options = *base_options;
	options.flags |= GIT_DIFF_FORCE_TEXT;
	int rc = git_diff_buffers(old_data, old_len, delta->old_file.path,
				  new_data, new_len, delta->new_file.path,
				  &options, ignore_file_cb, NULL, hunk_cb, line_cb, sink);
	git_blob_free(old_blob);
	git_blob_free(new_blob);
	return rc;
}
#endif

static int warm_binary_flags(git_diff *diff)
{
	for (size_t i = 0; i < git_diff_num_deltas(diff); i++) {
		git_patch *patch = NULL;
		int rc = git_patch_from_diff(&patch, diff, i);
		git_patch_free(patch);
		if (rc < 0)
			return rc;
	}
	return 0;
}

static int emit_diff(engine *e, uint64_t batch, const git_oid *commit_oid,
		     git_tree *old_tree, git_tree *new_tree,
		     uint64_t *files, uint64_t *hunks)
{
	git_diff *diff = NULL;
	git_diff_options options;
	if (git_diff_options_init(&options, GIT_DIFF_OPTIONS_VERSION) < 0)
		return -1;
	options.context_lines = 0;
	options.interhunk_lines = BL_INTERHUNK_LINES;
	options.flags = BL_DIFF_FLAGS;
	if (e->all_statuses)
		options.flags |= GIT_DIFF_INCLUDE_TYPECHANGE;
	if (e->find_copies)
		options.flags |= GIT_DIFF_INCLUDE_UNMODIFIED;
	if (git_diff_tree_to_tree(&diff, e->repo, old_tree, new_tree, &options) < 0)
		goto fail;
	git_diff_find_options find;
	if (git_diff_find_options_init(&find, GIT_DIFF_FIND_OPTIONS_VERSION) < 0)
		goto fail;
	find.flags = BL_FIND_FLAGS;
	find.rename_threshold = BL_RENAME_THRESHOLD;
	find.rename_limit = BL_RENAME_LIMIT;
#ifdef BL_CANONICAL_SIMILARITY
	git_diff_similarity_metric metric = {
		.file_signature = canonical_file_signature,
		.buffer_signature = canonical_buffer_signature,
		.free_signature = canonical_free_signature,
		.similarity = canonical_similarity,
		.payload = e,
	};
	find.metric = &metric;
#endif
	if (e->find_copies)
		find.flags |= GIT_DIFF_FIND_COPIES | GIT_DIFF_FIND_COPIES_FROM_UNMODIFIED |
			      GIT_DIFF_FIND_REMOVE_UNMODIFIED;
	if (git_diff_find_similar(diff, &find) < 0 || warm_binary_flags(diff) < 0)
		goto fail;
#ifdef BL_DEBUG_RENAMES
	for (size_t i = 0; i < git_diff_num_deltas(diff); i++) {
		const git_diff_delta *delta = git_diff_get_delta(diff, i);
		if (delta && delta->status == GIT_DELTA_RENAMED)
			fprintf(stderr, "rename score=%u %s -> %s\n", delta->similarity,
				delta->old_file.path, delta->new_file.path);
	}
#endif
	diff_sink sink = {
		.eng = e,
		.batch = batch,
		.commit_oid = commit_oid,
	};
	int rc = 0;
#ifdef BL_PUBLIC_TRIM_DIFF
	for (size_t i = 0; !rc && i < git_diff_num_deltas(diff); i++) {
		const git_diff_delta *delta = git_diff_get_delta(diff, i);
		char status = delta ? status_char(delta->status) : 0;
		if (!status || !allowed_status(e, status))
			continue;
		rc = file_cb(delta, 0, &sink);
		bool binary = (delta->flags & GIT_DIFF_FLAG_BINARY) != 0 &&
			      !git_oid_equal(&delta->old_file.id, &delta->new_file.id);
		if (!rc && !binary && !git_oid_equal(&delta->old_file.id, &delta->new_file.id))
			rc = emit_buffer_hunks(&sink, delta, &options);
	}
#else
	rc = git_diff_foreach(diff, file_cb, binary_cb, hunk_cb, line_cb, &sink);
#endif
	if (!rc)
		rc = close_file(&sink);
	buffer_dispose(&sink.hunk_added);
	if (rc) {
		git_diff_free(diff);
		return -1;
	}
	*files += sink.files;
	*hunks += sink.hunks;
	git_diff_free(diff);
	return 0;
fail:
	git_diff_free(diff);
	return -1;
}

static size_t utf8_codepoint(const unsigned char *data, size_t len, uint32_t *value)
{
	if (!len)
		return 0;
	if (data[0] < 0x80) {
		*value = data[0];
		return 1;
	}
	size_t width;
	uint32_t codepoint;
	if ((data[0] & 0xe0) == 0xc0) {
		width = 2;
		codepoint = data[0] & 0x1f;
	} else if ((data[0] & 0xf0) == 0xe0) {
		width = 3;
		codepoint = data[0] & 0x0f;
	} else if ((data[0] & 0xf8) == 0xf0) {
		width = 4;
		codepoint = data[0] & 0x07;
	} else {
		*value = UINT32_MAX;
		return 1;
	}
	if (width > len) {
		*value = UINT32_MAX;
		return 1;
	}
	for (size_t i = 1; i < width; i++) {
		if ((data[i] & 0xc0) != 0x80) {
			*value = UINT32_MAX;
			return 1;
		}
		codepoint = (codepoint << 6) | (data[i] & 0x3f);
	}
	if ((width == 2 && codepoint < 0x80) ||
	    (width == 3 && codepoint < 0x800) ||
	    (width == 4 && codepoint < 0x10000) ||
	    (codepoint >= 0xd800 && codepoint <= 0xdfff) || codepoint > 0x10ffff) {
		*value = UINT32_MAX;
		return 1;
	}
	*value = codepoint;
	return width;
}

static bool unicode_space(uint32_t value)
{
	return value == 0x09 || value == 0x0a || value == 0x0b ||
	       value == 0x0c || value == 0x0d || value == 0x20 ||
	       value == 0x85 || value == 0xa0 || value == 0x1680 ||
	       (value >= 0x2000 && value <= 0x200a) || value == 0x2028 ||
	       value == 0x2029 || value == 0x202f || value == 0x205f ||
	       value == 0x3000;
}

static void trim_unicode(const unsigned char *data, size_t len, bool left,
			 size_t *start_out, size_t *len_out)
{
	size_t start = 0;
	if (left) {
		while (start < len) {
			uint32_t value;
			size_t width = utf8_codepoint(data + start, len - start, &value);
			if (!unicode_space(value))
				break;
			start += width;
		}
	}
	size_t pos = start, end = start;
	while (pos < len) {
		uint32_t value;
		size_t width = utf8_codepoint(data + pos, len - pos, &value);
		pos += width;
		if (!unicode_space(value))
			end = pos;
	}
	*start_out = start;
	*len_out = end - start;
}

static int expand_message_tabs(buffer *out, const unsigned char *data, size_t len)
{
	size_t column = 0;
	for (size_t i = 0; i < len;) {
		if (data[i] == '\t') {
			size_t spaces = 8 - column % 8;
			for (size_t j = 0; j < spaces; j++) {
				if (buffer_u8(out, ' ') < 0)
					return -1;
			}
			column += spaces;
			i++;
			continue;
		}
		uint32_t value;
		size_t width = utf8_codepoint(data + i, len - i, &value);
		(void)value;
		if (buffer_add(out, data + i, width) < 0)
			return -1;
		column++;
		i += width;
	}
	return 0;
}

static int normalize_commit_message_bytes(buffer *out, const unsigned char *raw,
				  size_t raw_len)
{
	buffer title = {0}, body = {0}, expanded = {0};
	bool in_title = true, skip_body = false;
	size_t blank = 0;
	for (size_t pos = 0; pos <= raw_len;) {
		size_t end = pos;
		while (end < raw_len && raw[end] != '\n')
			end++;
		expanded.len = 0;
		if (expand_message_tabs(&expanded, raw + pos, end - pos) < 0)
			goto fail;
		size_t start, length;
		trim_unicode(expanded.data, expanded.len, in_title, &start, &length);
		if (in_title) {
			if (!length) {
				skip_body = title.len == 0;
				in_title = false;
			} else {
				if (title.len && buffer_u8(&title, ' ') < 0)
					goto fail;
				if (buffer_add(&title, expanded.data + start, length) < 0)
					goto fail;
			}
		} else if (!skip_body) {
			if (!length) {
				blank++;
			} else {
				if (body.len) {
					if (buffer_u8(&body, '\n') < 0)
						goto fail;
					if (blank && buffer_u8(&body, '\n') < 0)
						goto fail;
				}
				blank = 0;
				if (buffer_add(&body, expanded.data + start, length) < 0)
					goto fail;
			}
		}
		if (end == raw_len)
			break;
		pos = end + 1;
	}
	if (buffer_add(out, title.data, title.len) < 0)
		goto fail;
	if (title.len && body.len &&
	    (buffer_add(out, "\n\n", 2) < 0 || buffer_add(out, body.data, body.len) < 0))
		goto fail;
	buffer_dispose(&title);
	buffer_dispose(&body);
	buffer_dispose(&expanded);
	return 0;
fail:
	buffer_dispose(&title);
	buffer_dispose(&body);
	buffer_dispose(&expanded);
	return -1;
}

static int normalize_commit_message(buffer *out, const git_commit *commit)
{
	const unsigned char *raw = (const unsigned char *)git_commit_message_raw(commit);
	if (!raw)
		return -1;
	return normalize_commit_message_bytes(out, raw, strlen((const char *)raw));
}

typedef struct {
	git_odb_object *object;
	git_oid tree;
	git_oid first_parent;
	unsigned int parents;
	const unsigned char *message;
	size_t message_len;
	bool has_author;
	bool has_committer;
	bool supported_encoding;
} raw_commit;

static bool bytes_equal_ignore_case(const unsigned char *data, size_t len,
				    const char *expected)
{
	size_t expected_len = strlen(expected);
	if (len != expected_len)
		return false;
	for (size_t i = 0; i < len; i++) {
		unsigned char left = data[i];
		unsigned char right = (unsigned char)expected[i];
		if (left >= 'A' && left <= 'Z')
			left = (unsigned char)(left - 'A' + 'a');
		if (right >= 'A' && right <= 'Z')
			right = (unsigned char)(right - 'A' + 'a');
		if (left != right)
			return false;
	}
	return true;
}

static int parse_oid_header(git_oid *out, const unsigned char *line, size_t len,
			    const char *name, size_t oid_len)
{
	size_t name_len = strlen(name);
	size_t hex_len = oid_len * 2;
	if (len != name_len + 1 + hex_len || memcmp(line, name, name_len) ||
	    line[name_len] != ' ')
		return -1;
	return git_oid_fromstrn(out, (const char *)line + name_len + 1, hex_len);
}

static int read_raw_commit(engine *e, const git_oid *oid, raw_commit *out)
{
	memset(out, 0, sizeof(*out));
	out->supported_encoding = true;
	if (git_odb_read(&out->object, e->odb, oid) < 0)
		return -1;
	if (git_odb_object_type(out->object) != GIT_OBJECT_COMMIT)
		goto malformed;
	const unsigned char *data = git_odb_object_data(out->object);
	size_t size = git_odb_object_size(out->object);
	size_t pos = 0;
	bool have_tree = false;
	while (pos < size) {
		const unsigned char *newline = memchr(data + pos, '\n', size - pos);
		if (!newline)
			goto malformed;
		size_t line_len = (size_t)(newline - (data + pos));
		const unsigned char *line = data + pos;
		pos += line_len + 1;
		if (!have_tree) {
			if (parse_oid_header(&out->tree, line, line_len, "tree",
					     e->oid_len) < 0)
				goto malformed;
			have_tree = true;
			continue;
		}
		if (line_len > 7 && !memcmp(line, "parent ", 7)) {
			git_oid parent;
			if (parse_oid_header(&parent, line, line_len, "parent",
					     e->oid_len) < 0)
				goto malformed;
			if (!out->parents)
				git_oid_cpy(&out->first_parent, &parent);
			out->parents++;
			continue;
		}
		if (!line_len) {
			out->message = data + pos;
			out->message_len = size - pos;
			if (!out->has_committer)
				goto malformed;
			return 0;
		}
		if (line_len > 7 && !memcmp(line, "author ", 7))
			out->has_author = true;
		else if (line_len > 10 && !memcmp(line, "committer ", 10))
			out->has_committer = true;
		else if (line_len >= 9 && !memcmp(line, "encoding ", 9)) {
			const unsigned char *encoding = line + 9;
			size_t encoding_len = line_len - 9;
			out->supported_encoding =
				!encoding_len ||
				bytes_equal_ignore_case(encoding, encoding_len, "UTF-8") ||
				bytes_equal_ignore_case(encoding, encoding_len, "UTF8");
		}
	}
malformed:
	git_odb_object_free(out->object);
	memset(out, 0, sizeof(*out));
	return -1;
}

static void raw_commit_dispose(raw_commit *commit)
{
	git_odb_object_free(commit->object);
	memset(commit, 0, sizeof(*commit));
}

static int emit_missing_author_commit(engine *e, uint64_t batch,
				      const git_oid *oid, const raw_commit *commit,
				      uint64_t *files, uint64_t *hunks)
{
	buffer message = {0};
	if (normalize_commit_message_bytes(&message, commit->message,
					   commit->message_len) < 0)
		return -1;
	buffer b = {0};
	int rc = add_oid_bytes(&b, oid, e->oid_len);
	if (!rc)
		rc = buffer_bytes(&b, message.data, message.len);
	if (!rc)
		rc = buffer_bytes(&b, "", 0);
	if (!rc)
		rc = buffer_bytes(&b, "", 0);
	if (!rc)
		rc = buffer_u64(&b, 0);
	if (!rc)
		rc = buffer_u32(&b, 0);
	if (!rc)
		rc = buffer_u8(&b, 0);
	if (!rc)
		rc = buffer_u8(&b, 0);
	if (!rc)
		rc = write_frame("CMIT", batch, b.data, b.len);
	buffer_dispose(&b);
	buffer_dispose(&message);
	if (rc)
		return -1;

	git_tree *new_tree = NULL, *old_tree = NULL;
	if (commit->parents <= 1) {
		raw_commit parent = {0};
		if (git_tree_lookup(&new_tree, e->repo, &commit->tree) < 0)
			goto fail;
		if (commit->parents == 1) {
			if (read_raw_commit(e, &commit->first_parent, &parent) < 0 ||
			    git_tree_lookup(&old_tree, e->repo, &parent.tree) < 0) {
				raw_commit_dispose(&parent);
				goto fail;
			}
		}
		rc = emit_diff(e, batch, oid, old_tree, new_tree, files, hunks);
		raw_commit_dispose(&parent);
		git_tree_free(old_tree);
		git_tree_free(new_tree);
		if (rc < 0)
			return -1;
	}
	return write_empty("CEND", batch);
fail:
	git_tree_free(old_tree);
	git_tree_free(new_tree);
	return -1;
}

static int emit_commit(engine *e, uint64_t batch, git_commit *commit,
		       uint64_t *files, uint64_t *hunks)
{
	const git_signature *author = git_commit_author(commit);
	if (!author)
		return -1;
	const char *author_name = author->name ? author->name : "";
	const char *author_email = author->email ? author->email : "";
	if (git_mailmap_resolve(&author_name, &author_email, e->mailmap,
				author_name, author_email) < 0)
		return -1;
	buffer message = {0};
	if (normalize_commit_message(&message, commit) < 0)
		goto fail;
	buffer b = {0};
	int rc = add_oid_bytes(&b, git_commit_id(commit), e->oid_len);
	if (!rc)
		rc = buffer_bytes(&b, message.data, message.len);
	if (!rc)
		rc = buffer_bytes(&b, author_name, strlen(author_name));
	if (!rc)
		rc = buffer_bytes(&b, author_email, strlen(author_email));
	if (!rc)
		rc = buffer_u64(&b, author ? (uint64_t)author->when.time : 0);
	if (!rc)
		rc = buffer_u32(&b, author ? (uint32_t)author->when.offset : 0);
	if (!rc)
		rc = buffer_u8(&b, author != NULL);
	if (!rc)
		rc = buffer_u8(&b, author != NULL);
	if (!rc)
		rc = write_frame("CMIT", batch, b.data, b.len);
	buffer_dispose(&b);
	buffer_dispose(&message);
	if (rc)
		goto fail_commit;

	unsigned int parents = git_commit_parentcount(commit);
	if (parents <= 1) {
		git_tree *new_tree = NULL;
		git_tree *old_tree = NULL;
		git_commit *parent = NULL;
		if (git_commit_tree(&new_tree, commit) < 0)
			goto fail_commit;
		if (parents == 1) {
			if (git_commit_parent(&parent, commit, 0) < 0 ||
			    git_commit_tree(&old_tree, parent) < 0) {
				git_tree_free(new_tree);
				git_commit_free(parent);
				goto fail_commit;
			}
		}
		rc = emit_diff(e, batch, git_commit_id(commit), old_tree, new_tree,
			       files, hunks);
		git_tree_free(old_tree);
		git_tree_free(new_tree);
		git_commit_free(parent);
		if (rc < 0)
			goto fail_commit;
	}
	return write_empty("CEND", batch);
fail:
	buffer_dispose(&message);
fail_commit:
	return -1;
}

static bool supported_encoding(const git_commit *commit)
{
	const char *encoding = git_commit_message_encoding(commit);
	return !encoding || !*encoding || strcasecmp(encoding, "UTF-8") == 0 ||
	       strcasecmp(encoding, "UTF8") == 0;
}

static int process_batch(engine *e, const frame *f)
{
	if (!f->batch || f->payload.len < 4)
		return error_and_fail(f->batch, 1, "BGIN", "invalid batch header");
	const unsigned char *p = f->payload.data;
	size_t remain = f->payload.len;
	uint32_t count = be32(p);
	p += 4;
	remain -= 4;
	if ((uint64_t)count * (4u + e->oid_len) != remain)
		return error_and_fail(f->batch, 1, "BGIN",
				      "invalid commit count or OID width");
	git_commit **commits = calloc(count ? count : 1, sizeof(*commits));
	raw_commit *raw_commits = calloc(count ? count : 1, sizeof(*raw_commits));
	git_oid *oids = calloc(count ? count : 1, sizeof(*oids));
	if (!commits || !raw_commits || !oids) {
		free(commits);
		free(raw_commits);
		free(oids);
		return error_and_fail(f->batch, 2, "allocate", "out of memory");
	}
	int failed = 0;
	for (uint32_t i = 0; i < count; i++) {
		uint32_t n = be32(p);
		p += 4;
		if (n != e->oid_len || decode_oid(&oids[i], p, n, e->oid_len) < 0) {
			failed = error_and_fail(f->batch, 1, "BGIN", "invalid raw OID");
			break;
		}
		for (uint32_t j = 0; j < i; j++) {
			if (git_oid_equal(&oids[i], &oids[j])) {
				failed = error_and_fail(f->batch, 1, "BGIN", "duplicate OID");
				break;
			}
		}
		if (failed)
			break;
		p += n;
		if (git_commit_lookup(&commits[i], e->repo, &oids[i]) < 0) {
			if (read_raw_commit(e, &oids[i], &raw_commits[i]) < 0 ||
			    raw_commits[i].has_author) {
				failed = error_and_fail(f->batch, 2, "commit lookup",
						"object is not a valid commit with an optional author");
				break;
			}
			if (!raw_commits[i].supported_encoding) {
				failed = error_and_fail(f->batch, 2, "commit encoding",
						"non-UTF-8 commit encoding is unsupported");
				break;
			}
			continue;
		}
		if (!supported_encoding(commits[i])) {
			failed = error_and_fail(f->batch, 2, "commit encoding",
						"non-UTF-8 commit encoding is unsupported");
			break;
		}
	}
	if (!failed && write_empty("BGIN", f->batch) < 0)
		failed = -1;
	uint64_t files = 0, hunks = 0, emitted = 0;
	for (uint32_t i = 0; !failed && i < count; i++) {
		int rc = commits[i] ? emit_commit(e, f->batch, commits[i], &files, &hunks) :
			emit_missing_author_commit(e, f->batch, &oids[i], &raw_commits[i],
						   &files, &hunks);
		if (rc < 0) {
			failed = error_and_fail(f->batch, 2, "emit commit", last_git_error());
			break;
		}
		emitted++;
	}
	if (!failed) {
		unsigned char result[25] = {0};
		put64(result + 1, emitted);
		put64(result + 9, files);
		put64(result + 17, hunks);
		failed = write_frame("BEND", f->batch, result, sizeof(result));
	}
	for (uint32_t i = 0; i < count; i++) {
		git_commit_free(commits[i]);
		raw_commit_dispose(&raw_commits[i]);
	}
	free(commits);
	free(raw_commits);
	free(oids);
	return failed ? -1 : 0;
}

static void usage(const char *argv0)
{
	fprintf(stderr, "usage: %s --repo PATH [--all-statuses] [--find-copies]\n",
		argv0);
}

int main(int argc, char **argv)
{
	const char *repo_path = NULL;
	engine e = {0};
	for (int i = 1; i < argc; i++) {
		if (!strcmp(argv[i], "--repo") && i + 1 < argc)
			repo_path = argv[++i];
		else if (!strcmp(argv[i], "--all-statuses"))
			e.all_statuses = true;
		else if (!strcmp(argv[i], "--find-copies"))
			e.find_copies = true;
		else {
			usage(argv[0]);
			return 2;
		}
	}
	if (!repo_path) {
		usage(argv[0]);
		return 2;
	}
	if (git_libgit2_init() < 0)
		return 2;
	if (isolate_external_config() < 0 ||
	    git_libgit2_opts(GIT_OPT_SET_CACHE_MAX_SIZE,
			     (ssize_t)OBJECT_CACHE_LIMIT) < 0 ||
	    git_libgit2_opts(GIT_OPT_SET_MWINDOW_SIZE,
			     (size_t)MAP_WINDOW_SIZE) < 0 ||
	    git_libgit2_opts(GIT_OPT_SET_MWINDOW_MAPPED_LIMIT,
			     (size_t)MAPPED_LIMIT) < 0) {
		fprintf(stderr, "configure libgit2 process options: %s\n", last_git_error());
		git_libgit2_shutdown();
		return 2;
	}
	int exit_code = 1;
	if (git_repository_open_ext(&e.repo, repo_path, GIT_REPOSITORY_OPEN_NO_SEARCH,
				    NULL) < 0) {
		const char *message = last_git_error();
		if (strstr(message, "unknown object format")) {
			exit_code = write_error(0, 5, "repository object format", message) < 0 ? 1 : 0;
		} else {
			fprintf(stderr, "open repository: %s\n", message);
		}
		goto done;
	}
	if (git_repository_odb(&e.odb, e.repo) < 0) {
		fprintf(stderr, "open object database: %s\n", last_git_error());
		goto done;
	}
	git_oid_t oid_type = git_repository_oid_type(e.repo);
#ifdef GIT_EXPERIMENTAL_SHA256
	e.oid_len = oid_type == GIT_OID_SHA256 ? GIT_OID_SHA256_SIZE : GIT_OID_SHA1_SIZE;
#else
	if (oid_type != GIT_OID_SHA1) {
		exit_code = write_error(0, 5, "repository object format",
					"libgit2 build supports SHA-1 only") < 0 ? 1 : 0;
		goto done;
	}
	e.oid_len = GIT_OID_SHA1_SIZE;
#endif
	char reason[512];
	int preflight = preflight_config(e.repo, reason, sizeof(reason));
	if (preflight > 0) {
		exit_code = write_error(0, 5, "repository preflight", reason) < 0 ? 1 : 0;
		goto done;
	}
	if (preflight < 0) {
		fprintf(stderr, "%s\n", reason);
		goto done;
	}
	if (git_mailmap_from_repository(&e.mailmap, e.repo) < 0) {
		fprintf(stderr, "mailmap: %s\n", last_git_error());
		goto done;
	}
	if (write_hello(&e) < 0)
		goto done;
	frame f = {0};
	for (;;) {
		int rc = read_frame(&f);
		if (rc == 1)
			break;
		if (rc) {
			(void)write_error(0, 1, "read frame", "truncated, oversized, or invalid frame");
			break;
		}
		if (!memcmp(f.tag, "QUIT", 4)) {
			if (f.batch || f.payload.len)
				(void)write_error(f.batch, 1, "QUIT", "invalid QUIT frame");
			else
				exit_code = 0;
			break;
		}
		if (memcmp(f.tag, "BGIN", 4)) {
			(void)write_error(f.batch, 1, "request", "expected BGIN or QUIT");
			break;
		}
		if (process_batch(&e, &f) < 0)
			break;
	}
	buffer_dispose(&f.payload);
done:
	git_mailmap_free(e.mailmap);
	git_odb_free(e.odb);
	git_repository_free(e.repo);
	git_libgit2_shutdown();
	return exit_code;
}
