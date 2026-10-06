const {test} = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');

function fixture({native = false, supported = true, blocked = false} = {}) {
  class Video extends EventTarget {
    canPlayType() { return native ? 'maybe' : ''; }
    play() { return blocked ? Promise.reject({name: 'NotAllowedError'}) : Promise.resolve(); }
    pause() {}
    load() {}
    removeAttribute(name) { delete this[name]; }
    seekable = {length: 1, start: () => 10, end: () => 20};
  }
  const instances = [];
  class Hls {
    static isSupported = () => supported;
    static Events = {MANIFEST_PARSED: 'manifest', ERROR: 'error'};
    static ErrorTypes = {NETWORK_ERROR: 'network'};
    handlers = {};
    constructor() { instances.push(this); }
    on(event, handler) { this.handlers[event] = handler; }
    loadSource(url) { this.url = url; }
    attachMedia(video) { this.video = video; }
    destroy() { this.destroyed = true; }
  }
  const timers = new Map();
  let timerID = 0;
  const context = {window: {Hls}, AbortController, setTimeout(fn) { timers.set(++timerID, fn); return timerID; }, clearTimeout(id) { timers.delete(id); }};
  vm.runInNewContext(fs.readFileSync(__dirname + '/web/live.js', 'utf8'), context);
  const states = [];
  let expired = false;
  const video = new Video();
  const player = new context.window.DozorLivePlayer(video, (state) => states.push(state), () => { expired = true; });
  return {player, video, instances, timers, states, expired: () => expired};
}

test('switching and stopping release HLS and ignore callbacks from the old camera', () => {
  const f = fixture();
  f.player.start('/first/index.m3u8');
  const first = f.instances[0];
  f.player.start('/second/index.m3u8');
  assert.equal(first.destroyed, true);
  first.handlers.error(null, {fatal: true, type: 'network', response: {code: 401}});
  assert.equal(f.expired(), false);
  assert.equal(f.player.url, '/second/index.m3u8');
  f.player.stop();
  assert.equal(f.instances[1].destroyed, true);
  assert.equal(f.timers.size, 0);
  assert.equal(f.player.url, '');
});

test('network failures reconnect, then stopping cancels any retry', () => {
  const f = fixture();
  f.player.start('/live/index.m3u8');
  f.instances[0].handlers.error(null, {fatal: true, type: 'network'});
  assert.equal(f.states.at(-1), 'retrying');
  assert.equal(f.timers.size, 1);
  const retry = f.timers.values().next().value;
  f.timers.clear();
  retry();
  assert.equal(f.instances.length, 2);
  f.video.dispatchEvent(new Event('playing'));
  assert.equal(f.states.at(-1), 'playing');
  assert.equal(f.timers.size, 0);
  f.player.stop();
  f.video.dispatchEvent(new Event('waiting'));
  assert.equal(f.timers.size, 0);
});

test('expired sessions stop all requests and return to login', () => {
  const f = fixture();
  f.player.start('/live/index.m3u8');
  f.instances[0].handlers.error(null, {response: {code: 401}});
  assert.equal(f.expired(), true);
  assert.equal(f.instances[0].destroyed, true);
  assert.equal(f.timers.size, 0);
});

test('prefer MSE even when the browser advertises native HLS', () => {
  const f = fixture({native: true});
  f.player.start('/live/index.m3u8');
  assert.equal(f.instances.length, 1);
  assert.equal(f.instances[0].video, f.video);
  f.player.stop();
});

test('unsupported browsers and codec errors do not retry forever', () => {
  const f = fixture({supported: false});
  f.player.start('/live/index.m3u8');
  assert.equal(f.states.at(-1), 'error');
  assert.equal(f.timers.size, 0);
  const g = fixture();
  g.player.start('/live/index.m3u8');
  g.instances[0].handlers.error(null, {fatal: true, type: 'media'});
  assert.equal(g.states.at(-1), 'error');
  assert.equal(g.timers.size, 0);
});

test('native HLS, blocked autoplay and returning to the live edge', async () => {
  const f = fixture({native: true, supported: false, blocked: true});
  f.player.start('/live/index.m3u8');
  assert.equal(f.instances.length, 0);
  assert.equal(f.video.src, '/live/index.m3u8');
  f.video.dispatchEvent(new Event('loadedmetadata'));
  await Promise.resolve();
  assert.equal(f.states.at(-1), 'paused');
  assert.equal(f.timers.size, 0);
  f.player.goLive();
  assert.equal(f.video.currentTime, 19);
  f.player.stop();
  assert.equal(f.video.src, undefined);
});
