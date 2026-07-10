PKG_CONFIG ?= pkg-config
CC ?= cc
CFLAGS_COMMON := -std=c11 -D_POSIX_C_SOURCE=200809L -Wall -Wextra -Wpedantic -Werror \
	$(shell $(PKG_CONFIG) --cflags libgit2)
LDLIBS := $(shell $(PKG_CONFIG) --libs libgit2)
UNAME_S := $(shell uname -s)
ifeq ($(UNAME_S),Darwin)
SANITIZERS := undefined
else
SANITIZERS := address,undefined
endif

.PHONY: all clean native test sanitize

all: betterleaks-libgit2-engine

betterleaks-libgit2-engine: engine.c
	$(CC) $(CFLAGS_COMMON) -O2 $< -o $@ $(LDLIBS)

native: betterleaks-libgit2-engine-native

betterleaks-libgit2-engine-native: engine.c
	$(CC) $(CFLAGS_COMMON) -O2 -DBL_NATIVE_LIBGIT2_SEMANTICS $< -o $@ $(LDLIBS)

betterleaks-libgit2-engine-sanitize: engine.c
	$(CC) $(CFLAGS_COMMON) -O1 -g -fno-omit-frame-pointer \
		-fsanitize=$(SANITIZERS) $< -o $@ $(LDLIBS)

test: betterleaks-libgit2-engine
	python3 test_engine.py ./betterleaks-libgit2-engine
	go test ./interop -count=1

sanitize: betterleaks-libgit2-engine-sanitize
	BETTERLEAKS_SKIP_HUGE_HUNK=1 python3 test_engine.py ./betterleaks-libgit2-engine-sanitize

clean:
	rm -f betterleaks-libgit2-engine betterleaks-libgit2-engine-native \
		betterleaks-libgit2-engine-sanitize
