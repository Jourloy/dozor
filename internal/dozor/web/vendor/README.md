# Vendored browser player

- hls.js **1.7.3**, full build (including separate audio tracks), Apache-2.0: [upstream release](https://github.com/video-dev/hls.js/releases/tag/v1.7.3).
- `hls-1.7.3.min.js` is the unmodified `dist/hls.min.js` from [the pinned npm package](https://cdn.jsdelivr.net/npm/hls.js@1.7.3/dist/hls.min.js).
- SHA-256: `a12e7ee1cd64a69dcdb314157e45dafcba705bfb0b1440b7935cb265d374423e`.
- The upstream notice is preserved in `hls-LICENSE.txt`; the full license is in `Apache-2.0.txt`. Both are copied into release notices.

All files are embedded by Go; playback makes no CDN requests. `live.js` disables
workers to keep `script-src 'self'`; only `media-src` permits `blob:` for MSE.
When updating, replace the bundle and license, update the versioned script URL
and checksum, and run the player tests and a real RTSP/HLS playback check.
