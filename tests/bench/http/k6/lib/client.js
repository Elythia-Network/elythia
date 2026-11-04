import http from "k6/http";

// Misskey API client for k6 load testing.
// Mirrors the Python MisskeyLikeClient from federation tests.
export class MisskeyClient {
  constructor(baseUrl) {
    this.baseUrl = baseUrl;
    this.params = {
      headers: { "Content-Type": "application/json" },
    };
  }

  api(endpoint, body, token) {
    const payload = Object.assign({}, body || {});
    if (token) {
      payload.i = token;
    }
    return http.post(
      `${this.baseUrl}/api/${endpoint}`,
      JSON.stringify(payload),
      this.params,
    );
  }

  ping() {
    return this.api("ping");
  }

  meta() {
    return this.api("meta");
  }

  localTimeline(limit) {
    return this.api("notes/local-timeline", { limit: limit || 20 });
  }

  // homeTimeline is authenticated (token required) so it exercises the
  // viewer-dependent path: mute filter / myReaction / poll-vote resolution.
  homeTimeline(token, limit) {
    return this.api("notes/timeline", { limit: limit || 20 }, token);
  }

  createNote(text, token) {
    return this.api(
      "notes/create",
      { text: text, visibility: "public" },
      token,
    );
  }

  usersShow(username) {
    return this.api("users/show", { username: username });
  }

  i(token) {
    return this.api("i", {}, token);
  }

  notifications(token, limit) {
    return this.api("i/notifications", { limit: limit || 20 }, token);
  }

  notesShow(noteId, token) {
    return this.api("notes/show", { noteId: noteId }, token);
  }

  usersNotes(userId, token, limit) {
    return this.api("users/notes", { userId: userId, limit: limit || 20 }, token);
  }

  createReaction(noteId, reaction, token) {
    return this.api("notes/reactions/create", { noteId: noteId, reaction: reaction }, token);
  }

  // uploadFile sends force=true like the official frontend does, so a file
  // whose hash is already in the drive is stored again instead of being
  // answered with the existing one.
  uploadFile(name, bytes, mime, token) {
    return http.post(`${this.baseUrl}/api/drive/files/create`, {
      i: token,
      force: "true",
      file: http.file(bytes, name, mime),
    }, { timeout: this.params.timeout });
  }
}
