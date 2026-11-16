// Copyright 2026 The A2A Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Runs an ACP agent in a Docker sandbox whose workspace lives in memory,
// using the two C libraries without any other language:
//
//   example <lib dir> <image> <agent path in the image> <prompt>
//
// The ACP library asks to start the agent; the sandbox library starts it
// and hands over the file descriptor of its stdio. The program prints the
// reply and the files of the workspace. It parses JSON with string
// searches, which is enough for the fields it reads.

#include <dlfcn.h>
#include <pthread.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

typedef char *(*fn_s)(char *);
typedef char *(*fn_i)(int64_t);
typedef char *(*fn_is)(int64_t, char *);
typedef char *(*fn_ii)(int64_t, int32_t);
typedef char *(*fn_t)(int32_t);
typedef void (*fn_free)(char *);

static struct {
  fn_s open;
  fn_i start, close;
  fn_is list, read_file;
  fn_free free;
} sb;

static struct {
  fn_s new_;
  fn_i close;
  fn_is send, reply;
  fn_ii stream_next;
  fn_t next_event;
  fn_free free;
} acp;

static char sandbox_spec[4096];
static int64_t sandbox_handle;
static volatile int stopping;

static void *must(void *lib, const char *name) {
  void *p = dlsym(lib, name);
  if (p == NULL) {
    fprintf(stderr, "dlsym %s: %s\n", name, dlerror());
    exit(1);
  }
  return p;
}

// json_int returns the integer after "key": in s, or -1.
static long long json_int(const char *s, const char *key) {
  char pat[64];
  snprintf(pat, sizeof pat, "\"%s\":", key);
  const char *p = strstr(s, pat);
  return p == NULL ? -1 : atoll(p + strlen(pat));
}

static void fail_on_error(const char *what, const char *res) {
  if (strncmp(res, "{\"error\"", 8) == 0) {
    fprintf(stderr, "%s: %s\n", what, res);
    exit(1);
  }
}

// pump answers the questions of the ACP library: each "start" opens the
// sandbox on first use, starts the agent in it and answers with its fd.
static void *pump(void *arg) {
  (void)arg;
  while (!stopping) {
    char *ev = acp.next_event(250);
    if (strstr(ev, "\"type\":\"start\"") == NULL) {
      acp.free(ev);
      continue;
    }
    long long id = json_int(ev, "id");
    acp.free(ev);
    if (sandbox_handle == 0) {
      char *res = sb.open(sandbox_spec);
      fail_on_error("a2a_sandbox_open", res);
      sandbox_handle = json_int(res, "handle");
      sb.free(res);
    }
    char *res = sb.start(sandbox_handle);
    fail_on_error("a2a_sandbox_start", res);
    char answer[64];
    snprintf(answer, sizeof answer, "{\"fd\": %lld}", json_int(res, "fd"));
    sb.free(res);
    char *r = acp.reply(id, answer);
    fail_on_error("a2a_acp_reply", r);
    acp.free(r);
  }
  return NULL;
}

int main(int argc, char **argv) {
  if (argc != 5) {
    fprintf(stderr, "usage: %s <lib dir> <image> <agent path> <prompt>\n", argv[0]);
    return 2;
  }
  char path[1024];
  snprintf(path, sizeof path, "%s/liba2asandbox.so", argv[1]);
  void *lsb = dlopen(path, RTLD_NOW | RTLD_LOCAL);
  snprintf(path, sizeof path, "%s/liba2aacp.so", argv[1]);
  void *lacp = dlopen(path, RTLD_NOW | RTLD_LOCAL);
  if (lsb == NULL || lacp == NULL) {
    fprintf(stderr, "dlopen: %s\n", dlerror());
    return 1;
  }
  sb.open = (fn_s)must(lsb, "a2a_sandbox_open");
  sb.start = (fn_i)must(lsb, "a2a_sandbox_start");
  sb.close = (fn_i)must(lsb, "a2a_sandbox_close");
  sb.list = (fn_is)must(lsb, "a2a_sandbox_list");
  sb.read_file = (fn_is)must(lsb, "a2a_sandbox_read_file");
  sb.free = (fn_free)must(lsb, "a2a_sandbox_free");
  acp.new_ = (fn_s)must(lacp, "a2a_acp_new");
  acp.close = (fn_i)must(lacp, "a2a_acp_close");
  acp.send = (fn_is)must(lacp, "a2a_acp_send");
  acp.reply = (fn_is)must(lacp, "a2a_acp_reply");
  acp.stream_next = (fn_ii)must(lacp, "a2a_acp_stream_next");
  acp.next_event = (fn_t)must(lacp, "a2a_acp_next_event");
  acp.free = (fn_free)must(lacp, "a2a_acp_free");

  // seed.txt is base64 of "seeded from C".
  snprintf(sandbox_spec, sizeof sandbox_spec,
           "{\"name\": \"a2a-c-example-%d\", \"image\": \"%s\","
           " \"command\": {\"path\": \"%s\", \"dir\": \"/workspace\"},"
           " \"memory\": {\"dir\": \"/workspace\", \"files\": {\"seed.txt\": \"c2VlZGVkIGZyb20gQw==\"}},"
           " \"network\": {\"mode\": \"allowlist\"},"
           " \"launcher\": \"%s/a2a-sandbox-init\"}",
           (int)getpid(), argv[2], argv[3], argv[1]);

  pthread_t t;
  pthread_create(&t, NULL, pump, NULL);

  char *res = acp.new_("{\"id\": \"c\", \"external\": true, \"per_context\": true, \"session\": {\"cwd\": \"/workspace\"}}");
  fail_on_error("a2a_acp_new", res);
  int64_t exec = json_int(res, "handle");
  acp.free(res);

  char req[2048];
  snprintf(req, sizeof req, "{\"message\": {\"contextId\": \"c1\", \"parts\": [{\"text\": \"%s\"}]}}", argv[4]);
  res = acp.send(exec, req);
  fail_on_error("a2a_acp_send", res);
  int64_t stream = json_int(res, "stream");
  acp.free(res);
  for (;;) {
    char *ev = acp.stream_next(stream, 500);
    fail_on_error("a2a_acp_stream_next", ev);
    if (strstr(ev, "\"done\":true") != NULL) {
      acp.free(ev);
      break;
    }
    if (strstr(ev, "artifactUpdate") != NULL || strstr(ev, "TASK_STATE_COMPLETED") != NULL) {
      printf("event: %s\n", ev);
    }
    acp.free(ev);
  }

  res = sb.list(sandbox_handle, ".");
  printf("workspace: %s\n", res);
  sb.free(res);

  res = acp.close(exec);
  acp.free(res);
  stopping = 1;
  pthread_join(t, NULL);
  res = sb.close(sandbox_handle);
  fail_on_error("a2a_sandbox_close", res);
  sb.free(res);
  printf("done\n");
  return 0;
}
