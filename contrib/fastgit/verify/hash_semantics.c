/*
 * Standalone differential harness for the xdiff-xxh3 patch.
 *
 * Verifies, for the no-whitespace-flags fast path, the full observable
 * contract of xdl_hash_record OLD (DJB2 loop) vs NEW (memchr+XXH3):
 *
 *   C1. *data after the call is IDENTICAL (line advancement / framing).
 *       This is the only part of the return contract that feeds record
 *       boundaries (crec->size = cur - prev) and therefore diff output.
 *   C2. Hashed byte range [start, end) is identical (verified structurally
 *       by C1 + the range derivation, and dynamically via a logging shim).
 *   C3. hash is deterministic: same bytes -> same value, both hashers.
 *   C4. equal content  => equal hash (required for classifier dedup).
 *       (collisions -- different content, equal hash -- are allowed.)
 *   C5. guard-page adjacency: neither hasher reads at or past 'top' when
 *       the record has no trailing newline and 'top' sits exactly at an
 *       unmapped page boundary (catches SIMD overread).
 *
 * Inputs: exhaustive small cases + structured adversarial cases + random
 * fuzz with newline-dense alphabets.
 */
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <stdint.h>
#include <sys/mman.h>
#include <unistd.h>

#define XXH_INLINE_ALL
#define XXH_NO_STREAM
#if defined(__aarch64__)
#define XXH_VECTOR 0
#endif
#include "xxhash.h"

/* ---- OLD implementation (verbatim from vanilla xutils.c, fast path) ---- */
static unsigned long hash_old(char const **data, char const *top) {
	unsigned long ha = 5381;
	char const *ptr = *data;

	for (; ptr < top && *ptr != '\n'; ptr++) {
		ha += (ha << 5);
		ha ^= (unsigned long) *ptr;
	}
	*data = ptr < top ? ptr + 1 : ptr;
	return ha;
}

/* ---- NEW implementation (verbatim from the patch, fast path) ---- */
static unsigned long hash_new(char const **data, char const *top,
			      char const **h_start, size_t *h_len) {
	char const *ptr = *data;
	char const *nl, *end;

	nl = memchr(ptr, '\n', top - ptr);
	end = nl ? nl : top;
	*data = nl ? nl + 1 : top;

	*h_start = ptr; *h_len = (size_t)(end - ptr);   /* for C2/C5 checks */
	return (unsigned long) XXH3_64bits(ptr, (size_t)(end - ptr));
}

static long fails = 0, cases = 0;

static void check_buffer(const char *buf, size_t len, const char *tag) {
	/* walk the buffer as xdl_prepare_ctx does: cur < top loop */
	const char *top = buf + len;
	const char *cur_o = buf, *cur_n = buf;
	while (cur_o < top) {
		const char *rec_start = cur_o;
		const char *hs; size_t hl;
		unsigned long ho1, ho2, hn1, hn2;
		const char *save_o = cur_o, *save_n = cur_n;

		ho1 = hash_old(&cur_o, top);
		hn1 = hash_new(&cur_n, top, &hs, &hl);
		cases++;

		/* C1: framing identical */
		if (cur_o != cur_n) {
			fprintf(stderr, "FAIL C1[%s]: *data mismatch at rec off %zd (old adv %zd, new adv %zd)\n",
				tag, rec_start - buf, cur_o - save_o, cur_n - save_n);
			fails++;
			return; /* framing diverged; rest is garbage */
		}
		/* C2: hashed range = [rec_start, line end) */
		{
			const char *want_end = memchr(rec_start, '\n', top - rec_start);
			if (!want_end) want_end = top;
			if (hs != rec_start || hl != (size_t)(want_end - rec_start)) {
				fprintf(stderr, "FAIL C2[%s]: hashed range wrong at off %zd\n", tag, rec_start - buf);
				fails++;
			}
		}
		/* C3: determinism (re-run both on same record) */
		{
			const char *t1 = rec_start, *t2 = rec_start; const char *xs; size_t xl;
			ho2 = hash_old(&t1, top);
			hn2 = hash_new(&t2, top, &xs, &xl);
			if (ho1 != ho2 || hn1 != hn2) {
				fprintf(stderr, "FAIL C3[%s]: nondeterministic hash at off %zd\n", tag, rec_start - buf);
				fails++;
			}
		}
	}
	/* both cursors must land exactly on top */
	if (cur_o != top || cur_n != top) {
		fprintf(stderr, "FAIL C1-final[%s]: cursors old=%zd new=%zd len=%zu\n",
			tag, cur_o - buf, cur_n - buf, len);
		fails++;
	}
}

/* C4: equal content => equal hash, for random duplicate lines */
static void check_c4(void) {
	char line[4096];
	for (int trial = 0; trial < 100000; trial++) {
		size_t n = (size_t)(rand() % 4096);
		for (size_t i = 0; i < n; i++) {
			line[i] = (char)(rand() % 256);
			if (line[i] == '\n') line[i] = 'x';
		}
		/* two separately-allocated copies (different addresses/alignments) */
		size_t off1 = (size_t)(rand() % 64), off2 = (size_t)(rand() % 64);
		char *b1 = malloc(n + off1 + 1), *b2 = malloc(n + off2 + 1);
		memcpy(b1 + off1, line, n);
		memcpy(b2 + off2, line, n);
		const char *p1 = b1 + off1, *p2 = b2 + off2, *hs; size_t hl;
		unsigned long h1 = hash_new(&p1, b1 + off1 + n, &hs, &hl);
		unsigned long h2 = hash_new(&p2, b2 + off2 + n, &hs, &hl);
		if (h1 != h2) {
			fprintf(stderr, "FAIL C4: equal content, unequal XXH3 (len %zu, offs %zu/%zu)\n", n, off1, off2);
			fails++;
		}
		unsigned long o1, o2;
		p1 = b1 + off1; p2 = b2 + off2;
		o1 = hash_old(&p1, b1 + off1 + n);
		o2 = hash_old(&p2, b2 + off2 + n);
		if (o1 != o2) { fprintf(stderr, "FAIL C4-old baseline broken?!\n"); fails++; }
		free(b1); free(b2);
		cases++;
	}
}

/* C5: guard page — record without trailing newline ending exactly at an
 * unmapped page. Any read at/past 'top' segfaults. */
static void check_c5(void) {
	long pg = sysconf(_SC_PAGESIZE);
	/* map 3 pages, unmap the last -> [base, base+2pg) valid, base+2pg is a wall */
	char *base = mmap(NULL, (size_t)(3 * pg), PROT_READ | PROT_WRITE,
			  MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
	if (base == MAP_FAILED) { perror("mmap"); exit(2); }
	if (munmap(base + 2 * pg, (size_t)pg) != 0) { perror("munmap"); exit(2); }
	char *wall = base + 2 * pg;

	/* every record length 0..2*pg-1 (no newline anywhere), end flush at wall */
	for (long n = 0; n < 2 * pg; n++) {
		char *start = wall - n;
		if (n) memset(start, 'A' + (n % 26), (size_t)n);
		const char *p = start, *hs; size_t hl;
		unsigned long hn = hash_new(&p, wall, &hs, &hl);
		(void)hn;
		if (p != wall) { fprintf(stderr, "FAIL C5: cursor %p != wall %p (n=%ld)\n", (void*)p, (void*)wall, n); fails++; }
		p = start;
		unsigned long ho = hash_old(&p, wall);
		(void)ho;
		if (p != wall) { fprintf(stderr, "FAIL C5-old: cursor mismatch n=%ld\n", n); fails++; }
		cases += 2;
		/* also: newline exactly at wall-1 (line body flush against wall) */
		if (n >= 1) {
			start[n - 1] = '\n';
			p = start;
			hash_new(&p, wall, &hs, &hl);
			if (p != wall) { fprintf(stderr, "FAIL C5-nl: n=%ld\n", n); fails++; }
			const char *q = start;
			hash_old(&q, wall);
			if (q != p) { fprintf(stderr, "FAIL C5-nl-frame: n=%ld\n", n); fails++; }
			start[n - 1] = 'A';
			cases += 2;
		}
	}
	munmap(base, (size_t)(2 * pg));
	fprintf(stderr, "C5 guard-page sweep done (%ld page bytes)\n", pg);
}

int main(void) {
	srand(0xB16B00B5);

	/* exhaustive tiny cases: all buffers of len 0..3 over {A, \n, \0, \r} */
	{
		const char alpha[] = { 'A', '\n', '\0', '\r' };
		char buf[4];
		for (int len = 0; len <= 3; len++) {
			int total = 1;
			for (int i = 0; i < len; i++) total *= 4;
			for (int v = 0; v < total; v++) {
				int x = v;
				for (int i = 0; i < len; i++) { buf[i] = alpha[x % 4]; x /= 4; }
				check_buffer(buf, (size_t)len, "tiny");
			}
		}
	}

	/* structured adversarial cases */
	{
		static char big[1 << 20];
		/* no trailing newline */
		memset(big, 'q', 1000); check_buffer(big, 1000, "no-trailing-nl");
		/* only newlines */
		memset(big, '\n', 4096); check_buffer(big, 4096, "all-nl");
		/* CRLF */
		for (int i = 0; i < 4096; i += 2) { big[i] = '\r'; big[i+1] = '\n'; }
		check_buffer(big, 4096, "crlf");
		/* NUL-dense */
		memset(big, 0, 8192); for (int i = 100; i < 8192; i += 101) big[i] = '\n';
		check_buffer(big, 8192, "nul-dense");
		/* one giant line (1MB, no newline) */
		memset(big, 'Z', sizeof big); check_buffer(big, sizeof big, "1MB-line");
		/* giant line with newline at the very end */
		big[sizeof big - 1] = '\n'; check_buffer(big, sizeof big, "1MB-line-nl");
		/* high-bit bytes (sign-extension sensitivity of old hash on x86) */
		for (size_t i = 0; i < 65536; i++) big[i] = (char)(0x80 + (i % 128));
		for (size_t i = 77; i < 65536; i += 97) big[i] = '\n';
		check_buffer(big, 65536, "high-bit");
	}

	/* random fuzz: newline-dense random buffers, varied sizes */
	{
		static char buf[1 << 18];
		for (int trial = 0; trial < 2000; trial++) {
			size_t n = 1 + (size_t)(rand() % (1 << 18));
			for (size_t i = 0; i < n; i++) {
				int r = rand();
				buf[i] = (r % 7 == 0) ? '\n' : (char)(r >> 8);
			}
			check_buffer(buf, n, "fuzz");
		}
	}

	check_c4();
	check_c5();

	printf("cases=%ld fails=%ld\n", cases, fails);
	return fails ? 1 : 0;
}
