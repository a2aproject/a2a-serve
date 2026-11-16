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
//   example <lib dir> <image> <agent command> <prompt>
//           [--env NAME=VALUE]... [--allow RULE]...
//
// The agent command is split on spaces, such as "opencode acp".
// --env adds to the agent's environment, and --allow lets the sandbox
// reach a host, as --egress-allow does; everything else is refused. The
// ACP library asks to start the agent; the sandbox library starts it and
// hands over the file descriptor of its stdio. The program prints the
// reply and the files of the workspace. It parses JSON with string
// searches, which is enough for the fields it reads.

#include <dlfcn.h>
#include <stdatomic.h>
#include <pthread.h>
#include <signal.h>
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

static char sandbox_spec[65536];
// sandbox_handle is set before the pump thread starts, and read-only after.
static int64_t sandbox_handle;
static atomic_int stopping;

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

// interrupted is set once a signal starts removing the sandbox, and closed
// once the sandbox was removed.
static atomic_int interrupted, closed;

// close_sandbox removes the sandbox at exit, also after an error, once.
static void close_sandbox(void) {
  if (sandbox_handle != 0 && atomic_exchange(&closed, 1) == 0) {
    sb.free(sb.close(sandbox_handle));
  }
}

static void fail_on_error(const char *what, const char *res) {
  if (strncmp(res, "{\"error\"", 8) == 0) {
    // Closing the sandbox after a signal fails the calls in flight; the
    // signal's thread exits once it is done.
    while (atomic_load(&interrupted)) {
      pause();
    }
    fprintf(stderr, "%s: %s\n", what, res);
    exit(1);
  }
}

// append_json_string appends s to out as a JSON string.
static void append_json_string(char *out, size_t size, const char *s) {
  size_t n = strlen(out);
  if (n + 1 < size) out[n++] = '"';
  for (; *s != '\0' && n + 7 < size; s++) {
    unsigned char c = (unsigned char)*s;
    if (c == '"' || c == '\\') {
      out[n++] = '\\';
      out[n++] = (char)c;
    } else if (c < 0x20) {
      n += (size_t)snprintf(out + n, size - n, "\\u%04x", c);
    } else {
      out[n++] = (char)c;
    }
  }
  if (n + 1 < size) out[n++] = '"';
  out[n] = '\0';
}

// on_signal removes the sandbox when SIGINT or SIGTERM comes, which atexit
// doesn't see, then exits as the signal would.
static void *on_signal(void *arg) {
  sigset_t *set = arg;
  int sig;
  if (sigwait(set, &sig) == 0) {
    atomic_store(&interrupted, 1);
    close_sandbox();
    _exit(128 + sig);
  }
  return NULL;
}

// pump answers the questions of the ACP library: each "start" starts the
// agent in the sandbox and answers with its fd.
static void *pump(void *arg) {
  (void)arg;
  while (!atomic_load(&stopping)) {
    char *ev = acp.next_event(250);
    if (strstr(ev, "\"type\":\"start\"") == NULL) {
      acp.free(ev);
      continue;
    }
    long long id = json_int(ev, "id");
    acp.free(ev);
    char *res = sb.start(sandbox_handle);
    char answer[512];
    if (strncmp(res, "{\"error\"", 8) == 0) {
      snprintf(answer, sizeof answer, "%s", res);
    } else {
      snprintf(answer, sizeof answer, "{\"fd\": %lld}", json_int(res, "fd"));
    }
    sb.free(res);
    acp.free(acp.reply(id, answer));
  }
  return NULL;
}

int main(int argc, char **argv) {
  if (argc < 5 || (argc - 5) % 2 != 0) {
    fprintf(stderr, "usage: %s <lib dir> <image> <agent command> <prompt> [--env NAME=VALUE]... [--allow RULE]...\n", argv[0]);
    return 2;
  }
  // Every thread, those of the libraries included, inherits the blocked
  // signals, which on_signal then takes.
  static sigset_t signals;
  sigemptyset(&signals);
  sigaddset(&signals, SIGINT);
  sigaddset(&signals, SIGTERM);
  pthread_sigmask(SIG_BLOCK, &signals, NULL);
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
  char env[32768] = "", allow[4096] = "";
  for (int i = 5; i < argc; i += 2) {
    char *list = strcmp(argv[i], "--env") == 0 ? env : strcmp(argv[i], "--allow") == 0 ? allow : NULL;
    if (list == NULL) {
      fprintf(stderr, "unknown option %s\n", argv[i]);
      return 2;
    }
    size_t size = list == env ? sizeof env : sizeof allow;
    if (list[0] != '\0') strncat(list, ",", size - strlen(list) - 1);
    append_json_string(list, size, argv[i + 1]);
  }
  char agent[1024] = "", args[4096] = "";
  char *command = strdup(argv[3]);
  for (char *word = strtok(command, " "); word != NULL; word = strtok(NULL, " ")) {
    if (agent[0] == '\0') {
      append_json_string(agent, sizeof agent, word);
      continue;
    }
    if (args[0] != '\0') strncat(args, ",", sizeof args - strlen(args) - 1);
    append_json_string(args, sizeof args, word);
  }
  free(command);
  snprintf(sandbox_spec, sizeof sandbox_spec,
           "{\"name\": \"a2a-c-example-%d\", \"image\": \"%s\","
           " \"command\": {\"path\": %s, \"args\": [%s], \"dir\": \"/workspace\", \"env\": [%s]},"
           " \"memory\": {\"dir\": \"/workspace\", \"files\": {\"seed.txt\": \"c2VlZGVkIGZyb20gQw==\"}},"
           " \"network\": {\"mode\": \"allowlist\", \"allow\": [%s]},"
           " \"launcher\": \"%s/a2a-sandbox-init\"}",
           (int)getpid(), argv[2], agent, args, env, allow, argv[1]);

  atexit(close_sandbox);
  char *res = sb.open(sandbox_spec);
  fail_on_error("a2a_sandbox_open", res);
  sandbox_handle = json_int(res, "handle");
  sb.free(res);

  pthread_t t, st;
  pthread_create(&st, NULL, on_signal, &signals);
  pthread_detach(st);
  pthread_create(&t, NULL, pump, NULL);

  res = acp.new_("{\"id\": \"c\", \"external\": true, \"per_context\": true, \"session\": {\"cwd\": \"/workspace\"}}");
  fail_on_error("a2a_acp_new", res);
  int64_t exec = json_int(res, "handle");
  acp.free(res);

  char prompt[16384] = "", req[20480];
  append_json_string(prompt, sizeof prompt, argv[4]);
  snprintf(req, sizeof req, "{\"message\": {\"contextId\": \"c1\", \"parts\": [{\"text\": %s}]}}", prompt);
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
    if (strstr(ev, "artifactUpdate") != NULL || strstr(ev, "TASK_STATE_") != NULL) {
      printf("event: %s\n", ev);
    }
    acp.free(ev);
  }

  res = sb.list(sandbox_handle, ".");
  printf("workspace: %s\n", res);
  sb.free(res);

  res = acp.close(exec);
  acp.free(res);
  atomic_store(&stopping, 1);
  pthread_join(t, NULL);
  if (atomic_exchange(&closed, 1) == 0) {
    res = sb.close(sandbox_handle);
    fail_on_error("a2a_sandbox_close", res);
    sb.free(res);
  }
  while (atomic_load(&interrupted)) {
    pause();
  }
  printf("done\n");
  return 0;
}
