// SPDX-License-Identifier: AGPL-3.0-only

// diskfault slows or fails the writes and syncs that a process makes through
// libc on files under one folder. TestBreakDisk preloads it into the daemon,
// whose SQLite database writes through libc. Go's own file calls do not pass
// through it.
//
// DISKFAULT_DIR names the folder. DISKFAULT_CONTROL names a file with one
// line, "OPS DELAY_MS ERRNO PERCENT": OPS is sync, write or none; each
// matching call waits DELAY_MS, then fails with ERRNO in PERCENT of calls.
// The file is read again at most every 50 ms.

#define _GNU_SOURCE
#include <dlfcn.h>
#include <errno.h>
#include <limits.h>
#include <pthread.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/types.h>
#include <time.h>
#include <unistd.h>

static pthread_mutex_t lock = PTHREAD_MUTEX_INITIALIZER;
static char ops[16] = "none";
static long delay_ms, fail_errno, percent;
static struct timespec loaded;

static long since_ms(const struct timespec *then) {
	struct timespec now;
	clock_gettime(CLOCK_MONOTONIC, &now);
	return (now.tv_sec - then->tv_sec) * 1000 + (now.tv_nsec - then->tv_nsec) / 1000000;
}

// fault returns the errno a call of kind on fd fails with, or 0, after its
// delay.
static int fault(int fd, const char *kind) {
	const char *dir = getenv("DISKFAULT_DIR"), *control = getenv("DISKFAULT_CONTROL");
	if (dir == NULL || control == NULL) {
		return 0;
	}
	char link[64], path[PATH_MAX];
	snprintf(link, sizeof link, "/proc/self/fd/%d", fd);
	ssize_t n = readlink(link, path, sizeof path - 1);
	if (n < 0) {
		return 0;
	}
	path[n] = 0;
	if (strncmp(path, dir, strlen(dir)) != 0) {
		return 0;
	}
	char want[16];
	long delay, code, share;
	pthread_mutex_lock(&lock);
	if (loaded.tv_sec == 0 || since_ms(&loaded) > 50) {
		FILE *f = fopen(control, "r");
		if (f != NULL) {
			char o[16];
			long d, e, p;
			if (fscanf(f, "%15s %ld %ld %ld", o, &d, &e, &p) == 4) {
				strcpy(ops, o);
				delay_ms = d, fail_errno = e, percent = p;
			}
			fclose(f);
		}
		clock_gettime(CLOCK_MONOTONIC, &loaded);
	}
	strcpy(want, ops);
	delay = delay_ms, code = fail_errno, share = percent;
	pthread_mutex_unlock(&lock);
	if (strcmp(want, kind) != 0) {
		return 0;
	}
	if (delay > 0) {
		struct timespec wait = {delay / 1000, (delay % 1000) * 1000000};
		nanosleep(&wait, NULL);
	}
	if (code != 0 && rand() % 100 < share) {
		return (int)code;
	}
	return 0;
}

#define NEXT(name) static __typeof__(name) *next_##name; if (next_##name == NULL) next_##name = dlsym(RTLD_NEXT, #name)

ssize_t write(int fd, const void *buf, size_t count) {
	NEXT(write);
	int code = fault(fd, "write");
	if (code != 0) {
		errno = code;
		return -1;
	}
	return next_write(fd, buf, count);
}

ssize_t pwrite(int fd, const void *buf, size_t count, off_t offset) {
	NEXT(pwrite);
	int code = fault(fd, "write");
	if (code != 0) {
		errno = code;
		return -1;
	}
	return next_pwrite(fd, buf, count, offset);
}

ssize_t pwrite64(int fd, const void *buf, size_t count, off_t offset) {
	NEXT(pwrite64);
	int code = fault(fd, "write");
	if (code != 0) {
		errno = code;
		return -1;
	}
	return next_pwrite64(fd, buf, count, offset);
}

int fsync(int fd) {
	NEXT(fsync);
	int code = fault(fd, "sync");
	if (code != 0) {
		errno = code;
		return -1;
	}
	return next_fsync(fd);
}

int fdatasync(int fd) {
	NEXT(fdatasync);
	int code = fault(fd, "sync");
	if (code != 0) {
		errno = code;
		return -1;
	}
	return next_fdatasync(fd);
}
