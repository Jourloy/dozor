'use strict';

/** One live connection at a time, with bounded buffers and cancellation on every switch. */
window.DozorLivePlayer = class {
  constructor(video, onState, onExpired) {
    this.video = video;
    this.onState = onState;
    this.onExpired = onExpired;
    this.generation = 0;
    this.attempt = 0;
    this.url = '';
  }

  start(url) {
    this.stop();
    this.url = url;
    this.attempt = 0;
    this.connect();
  }

  stop() {
    this.url = '';
    this.release();
  }

  release() {
    this.generation += 1;
    clearTimeout(this.watchdog);
    clearTimeout(this.retryTimer);
    this.watchdog = null;
    if (this.listeners) this.listeners.abort();
    if (this.hls) this.hls.destroy();
    this.hls = null;
    this.video.pause();
    this.video.removeAttribute('src');
    this.video.load();
  }

  watch() {
    if (!this.watchdog) this.watchdog = setTimeout(() => this.fail(true), 20000);
  }

  fail(retry, message = '') {
    this.release();
    if (!retry) {
      this.onState('error', message || 'Браузер не смог воспроизвести поток. Выберите H.264 и звук AAC в настройках камеры.');
      return;
    }
    this.onState('retrying', 'Поток недоступен. Проверьте подключение камеры. Переподключаемся автоматически…');
    const delay = Math.min(1000 * 2 ** Math.min(this.attempt++, 4), 10000);
    this.retryTimer = setTimeout(() => this.connect(), delay);
  }

  connect() {
    if (!this.url) return;
    const generation = this.generation;
    const active = () => generation === this.generation && Boolean(this.url);
    this.listeners = new AbortController();
    const listen = (event, fn) => this.video.addEventListener(event, () => {
      if (active()) fn();
    }, {signal: this.listeners.signal});
    this.onState('connecting', 'Подключаемся к камере…');
    this.watch();
    const play = () => {
      if (!active()) return;
      this.video.play().catch(error => {
        if (!active() || error.name === 'AbortError') return;
        if (error.name === 'NotAllowedError') {
          clearTimeout(this.watchdog);
          this.watchdog = null;
          this.onState('paused', 'Нажмите кнопку воспроизведения на видео.');
        } else this.fail(false);
      });
    };
    listen('playing', () => {
      clearTimeout(this.watchdog);
      this.watchdog = null;
      this.attempt = 0;
      this.onState('playing', 'Прямая трансляция с небольшой задержкой.');
    });
    listen('waiting', () => {
      this.onState('connecting', 'Ожидаем изображение от камеры…');
      this.watch();
    });
    listen('pause', () => {
      clearTimeout(this.watchdog);
      this.watchdog = null;
      this.onState('paused', 'Просмотр на паузе. Нажмите «К эфиру», чтобы вернуться к текущему изображению.');
    });
    listen('ended', () => this.fail(true));
    listen('error', () => this.fail(this.video.error && this.video.error.code === 2));

    // Prefer MSE: some browsers advertise native HLS but cannot demux the
    // separate fMP4 audio/video playlists emitted by MediaMTX.
    if (window.Hls && window.Hls.isSupported()) {
      const Hls = window.Hls;
      const hls = this.hls = new Hls({
        // Keep script-src strict: a worker would require allowing blob scripts.
        enableWorker: false,
        lowLatencyMode: false,
        liveSyncDurationCount: 3,
        liveMaxLatencyDurationCount: 6,
        maxBufferLength: 10,
        maxMaxBufferLength: 20,
        backBufferLength: 0,
      });
      hls.on(Hls.Events.MANIFEST_PARSED, play);
      hls.on(Hls.Events.ERROR, (_, data) => {
        if (!active()) return;
        if (data.response && data.response.code === 401) {
          this.stop();
          this.onExpired();
        } else if (data.fatal) {
          this.fail(data.type === Hls.ErrorTypes.NETWORK_ERROR);
        }
      });
      hls.loadSource(this.url);
      hls.attachMedia(this.video);
    } else if (this.video.canPlayType('application/vnd.apple.mpegurl')) {
      listen('loadedmetadata', play);
      this.video.src = this.url;
    } else {
      this.fail(false, 'Этот браузер не поддерживает онлайн-видео. Откройте Dozor в актуальном Safari, Chrome, Edge или Firefox.');
    }
  }

  goLive() {
    if (!this.url) return;
    const position = this.hls && this.hls.liveSyncPosition;
    const ranges = this.video.seekable;
    if (Number.isFinite(position)) this.video.currentTime = position;
    else if (ranges.length) this.video.currentTime = Math.max(ranges.start(ranges.length - 1), ranges.end(ranges.length - 1) - 1);
    this.video.play().catch(() => {});
  }
};
