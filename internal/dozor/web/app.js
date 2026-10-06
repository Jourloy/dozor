'use strict';
/*
 * Dozor web UI. Classic scripts, no build step; HLS playback is isolated in live.js.
 * The server sends a strict CSP: no inline handlers or styles. Every dynamic style goes through the CSSOM
 * (el.style.setProperty), every text through textContent.
 *
 * Sections: helpers, formatting, api, toasts, dialogs, screens, router, overview, events and player,
 * cameras, settings, polling and start.
 */
(() => {
  // ------------------------------------------------------------------ helpers
  const $ = (selector, root = document) => root.querySelector(selector);
  const $$ = (selector, root = document) => Array.from(root.querySelectorAll(selector));
  const SVG_NS = 'http://www.w3.org/2000/svg';
  const NB = ' ';
  const reducedMotion = window.matchMedia('(prefers-reduced-motion: reduce)');

  /** Russian typography (00.md 5.5): a non-breaking space after short prepositions and conjunctions, before a dash, inside "number unit". */
  const SHORT_WORD = /(^|[\s(«„"])([вксуоаияВКСУОАИЯ]|[нН][аеоиу]|[пП]о|[иИ]з|[зЗ]а|[оО][тб]|[дД]о|[вВкКсС]о|[бБ]ез|[дД]ля|[пП]ри|[пП]од|[нН]ад|[пП]ро)\s+/g;
  function t(text) {
    let s = String(text);
    s = s.replace(SHORT_WORD, '$1$2' + NB).replace(SHORT_WORD, '$1$2' + NB);
    s = s.replace(/\s+—\s+/g, NB + '— ');
    s = s.replace(/(\d)\s+(?=[%°№А-Яа-яЁё])/g, '$1' + NB);
    return s;
  }
  const hasCyrillic = text => /[А-Яа-яЁё]/.test(text);
  const capitalize = text => text.charAt(0).toUpperCase() + text.slice(1);
  const sentence = text => (/[.!?…]$/.test(text) ? text : text + '.');

  /** Creates an element. props: class, text, on:{event:fn}, dataset:{}, any other key becomes an attribute (true = empty attribute). */
  function el(tag, props, ...children) {
    const node = document.createElement(tag);
    if (props) {
      for (const [key, value] of Object.entries(props)) {
        if (value == null || value === false) continue;
        if (key === 'class') node.className = value;
        else if (key === 'text') node.textContent = value;
        else if (key === 'dataset') {
          for (const [name, item] of Object.entries(value)) if (item != null && item !== false) node.dataset[name] = item === true ? '' : item;
        }
        else if (key === 'on') for (const [name, handler] of Object.entries(value)) node.addEventListener(name, handler);
        else node.setAttribute(key, value === true ? '' : value);
      }
    }
    for (const child of children.flat()) {
      if (child == null || child === false) continue;
      node.append(typeof child === 'object' ? child : document.createTextNode(child));
    }
    return node;
  }
  /** Inline SVG icon from the sprite in index.html. */
  function icon(name, options = {}) {
    const svg = document.createElementNS(SVG_NS, 'svg');
    svg.setAttribute('class', 'ui-icon');
    svg.setAttribute('aria-hidden', 'true');
    if (options.size) svg.setAttribute('data-size', String(options.size));
    if (options.slot) svg.setAttribute('data-icon', options.slot);
    const use = document.createElementNS(SVG_NS, 'use');
    use.setAttribute('href', '#i-' + name);
    svg.append(use);
    return svg;
  }
  function spinner() {
    const svg = document.createElementNS(SVG_NS, 'svg');
    svg.setAttribute('class', 'ui-spinner');
    svg.setAttribute('viewBox', '0 0 24 24');
    svg.setAttribute('aria-hidden', 'true');
    const use = document.createElementNS(SVG_NS, 'use');
    use.setAttribute('href', '#i-spinner');
    svg.append(use);
    return svg;
  }
  const badge = (text, variant) => el('span', {class: 'ui-badge', dataset: {variant}, text: t(text)});
  const field = (form, name) => form.elements.namedItem(name);
  const setText = (node, text) => {
    if (node.textContent !== text) node.textContent = text;
  };
  /** Rebuilds a container only when its data changed (the poll runs every 10 s and must not flicker). */
  function renderIfChanged(container, key, build) {
    const signature = JSON.stringify(key);
    if (container._signature === signature) return;
    container._signature = signature;
    build();
  }
  const announce = message => {
    const node = $('#announce');
    node.textContent = '';
    window.setTimeout(() => {
      node.textContent = message;
    }, 50);
  };

  // ------------------------------------------------------------------ formatting (Intl, ru-RU, the configured time zone)
  const DEFAULT_ZONE = 'Europe/Moscow';
  const number = new Intl.NumberFormat('ru-RU');
  const percent = new Intl.NumberFormat('ru-RU', {style: 'percent', maximumFractionDigits: 0});
  const plain = new Intl.NumberFormat('ru-RU', {useGrouping: false});
  const plural = new Intl.PluralRules('ru-RU');
  const pluralize = (n, forms) => {
    const rule = plural.select(n);
    return forms[rule === 'one' ? 0 : rule === 'few' ? 1 : 2];
  };
  const BYTE_UNITS = ['byte', 'kilobyte', 'megabyte', 'gigabyte', 'terabyte'];
  const byteFormats = BYTE_UNITS.map((unit, i) => new Intl.NumberFormat('ru-RU', {style: 'unit', unit, unitDisplay: 'short', maximumFractionDigits: i > 1 ? 1 : 0}));
  function bytes(value) {
    const n = Number(value);
    if (!Number.isFinite(n) || n <= 0) return byteFormats[0].format(0);
    const i = Math.min(4, Math.floor(Math.log(n) / Math.log(1024)));
    return byteFormats[i].format(n / 1024 ** i);
  }
  let zone = DEFAULT_ZONE;
  let dateTimeFormat, timeFormat, longDateFormat, fullFormat, isoDateFormat;
  function buildFormatters() {
    let name = (settings && settings.timezone) || DEFAULT_ZONE;
    try {
      new Intl.DateTimeFormat('ru-RU', {timeZone: name});
    } catch {
      name = DEFAULT_ZONE;
    }
    zone = name;
    dateTimeFormat = new Intl.DateTimeFormat('ru-RU', {timeZone: zone, day: '2-digit', month: 'short', hour: '2-digit', minute: '2-digit', second: '2-digit'});
    timeFormat = new Intl.DateTimeFormat('ru-RU', {timeZone: zone, hour: '2-digit', minute: '2-digit', second: '2-digit'});
    longDateFormat = new Intl.DateTimeFormat('ru-RU', {timeZone: zone, dateStyle: 'long'});
    fullFormat = new Intl.DateTimeFormat('ru-RU', {timeZone: zone, dateStyle: 'long', timeStyle: 'short'});
    // en-CA gives YYYY-MM-DD: the machine-readable date in the same zone as the text.
    isoDateFormat = new Intl.DateTimeFormat('en-CA', {timeZone: zone, year: 'numeric', month: '2-digit', day: '2-digit'});
  }
  // No line break between the day and the month of a date.
  const time = ms => dateTimeFormat.format(new Date(ms)).replace(/^(\d+)\s/, '$1' + NB);
  const clock = ms => timeFormat.format(new Date(ms));

  // ------------------------------------------------------------------ state
  let csrf = '';
  let settings = null;
  let snapshot = null; // last confirmed GET /status
  let lastOk = 0;
  let failures = 0;
  let polling = false;
  let screen = 'pending';
  let currentTab = null;
  const TABS = ['overview', 'live', 'events', 'cameras', 'settings'];
  const TITLES = {overview: t('Под присмотром'), live: 'Онлайн-просмотр', events: 'Архив событий', cameras: 'Камеры', settings: 'Настройки'};
  const screens = {pending: $('#pending'), fatal: $('#fatal'), login: $('#login'), shell: $('#shell')};

  // ------------------------------------------------------------------ errors (00.md 10.8: no raw technical text, no status codes)
  const MSG = {
    network: t('Нет связи с Dozor. Проверьте сеть и повторите.'),
    generic: t('Не удалось выполнить действие. Повторите попытку.'),
    eventsFailed: t('Не удалось загрузить события. Повторите попытку.'),
    sessionExpired: t('Сессия истекла. Войдите снова.'),
    sessionStale: t('Сессия устарела. Войдите снова.'),
  };
  class ApiError extends Error {
    constructor(message, status, handled) {
      super(message);
      this.status = status;
      this.handled = Boolean(handled);
    }
  }
  /** A message written by this script for the user (validation), safe to show as is. */
  class UserError extends Error {}
  function messageOf(error) {
    if (error instanceof ApiError || error instanceof UserError) return error.message;
    console.error(error);
    return MSG.generic;
  }
  /** Turns server text into something safe for the screen: curated Russian only, without internal ids. */
  function humanize(text, fallback) {
    const value = typeof text === 'string' ? text.trim() : '';
    if (value && value.length <= 240 && hasCyrillic(value) && !/[0-9a-f]{32}/i.test(value)) return sentence(capitalize(t(value)));
    return fallback;
  }

  async function fetchAuth() {
    let response;
    try {
      response = await fetch('/api/v1/auth', {headers: {'Content-Type': 'application/json', 'X-CSRF-Token': csrf}});
    } catch {
      throw new ApiError(MSG.network, 0);
    }
    let data = null;
    try {
      data = await response.json();
    } catch {
      /* non-JSON answer */
    }
    if (!response.ok || !data || typeof data.authenticated !== 'boolean') throw new ApiError(MSG.generic, response.status);
    return data;
  }

  /** One request to /api/v1. Every request carries the CSRF header; bodies are built by the callers field by field. */
  async function api(path, method = 'GET', body, options = {}) {
    let response;
    try {
      response = await fetch('/api/v1' + path, {
        method,
        headers: {'Content-Type': 'application/json', 'X-CSRF-Token': csrf},
        body: body === undefined ? undefined : JSON.stringify(body),
      });
    } catch {
      throw new ApiError(MSG.network, 0);
    }
    if (response.status === 204) return null;
    let data = null;
    try {
      data = await response.json();
    } catch {
      /* non-JSON answer */
    }
    if (response.ok) {
      if (data === null) throw new ApiError(options.fallback || MSG.generic, response.status);
      return data;
    }
    const serverText = data && typeof data.error === 'string' ? data.error : '';
    if (response.status === 401 && path !== '/login') {
      showLogin({notice: MSG.sessionExpired});
      throw new ApiError(MSG.sessionExpired, 401, true);
    }
    if (response.status === 403 && path !== '/login' && /csrf/i.test(serverText)) {
      // The token is gone (a restart, an old tab): ask who we are now.
      const auth = await fetchAuth();
      if (auth.authenticated && !options.retried) {
        csrf = auth.csrf;
        return api(path, method, body, {...options, retried: true});
      }
      showLogin({setup: auth.setup_required, notice: MSG.sessionStale});
      throw new ApiError(MSG.sessionStale, 403, true);
    }
    if (response.status >= 400 && response.status < 500 && serverText && hasCyrillic(serverText)) {
      throw new ApiError(sentence(capitalize(t(serverText))), response.status);
    }
    throw new ApiError(options.fallback || MSG.generic, response.status);
  }

  // ------------------------------------------------------------------ busy buttons
  function setBusy(button, busy) {
    if (!button) return;
    button.disabled = busy;
    if (busy) button.setAttribute('aria-busy', 'true');
    else button.removeAttribute('aria-busy');
  }
  /** Wraps an async handler: the button is disabled and aria-busy while it runs (the label stays), errors go to onError or a toast. */
  function run(handler, options = {}) {
    return async event => {
      if (event && event.preventDefault) event.preventDefault();
      const button = options.button || (event && event.submitter) || (event && event.currentTarget instanceof HTMLButtonElement ? event.currentTarget : null);
      if (button && button.getAttribute('aria-busy') === 'true') return;
      const hadFocus = Boolean(button) && document.activeElement === button;
      setBusy(button, true);
      try {
        await handler(event);
      } catch (error) {
        if (!(error && error.handled)) (options.onError || (e => toast(messageOf(e), 'error')))(error);
      } finally {
        setBusy(button, false);
        if (hadFocus && button.isConnected && document.activeElement === document.body) button.focus({preventScroll: true});
      }
    };
  }

  // ------------------------------------------------------------------ toasts: result of an action, polite, no focus change
  const TOAST = {
    success: {icon: 'circle-check', word: 'Готово: '},
    info: {icon: 'info', word: ''},
    warning: {icon: 'alert', word: 'Внимание: '},
    error: {icon: 'circle-alert', word: 'Ошибка: '},
  };
  const TOAST_LIMIT = 4;
  function closeToast(node) {
    if (!node.isConnected || node.hasAttribute('data-closing')) return;
    window.clearTimeout(node._timer);
    node.setAttribute('data-closing', '');
    window.setTimeout(() => node.remove(), reducedMotion.matches ? 0 : 160);
  }
  /** Errors stay until closed; everything else closes by itself after about 5 s (a hover or focus pauses the timer). */
  function toast(message, type = 'info', key) {
    const list = $('#toasts');
    if (key) $$('[data-key="' + key + '"]', list).forEach(closeToast);
    const kind = TOAST[type] || TOAST.info;
    const node = el(
      'div',
      {class: 'ui-toast', dataset: {type, key}},
      el('span', {class: 'ui-toast-icon'}, icon(kind.icon)),
      el('p', {class: 'ui-toast-text'}, kind.word ? el('span', {class: 'visually-hidden', text: kind.word}) : null, message),
      el('button', {type: 'button', class: 'ui-button ui-toast-close', dataset: {variant: 'ghost', size: 'icon-sm'}, 'aria-label': 'Закрыть уведомление', on: {click: () => closeToast(node)}}, icon('close'))
    );
    const arm = () => {
      if (type === 'error') return;
      window.clearTimeout(node._timer);
      node._timer = window.setTimeout(() => closeToast(node), 5000);
    };
    node.addEventListener('pointerenter', () => window.clearTimeout(node._timer));
    node.addEventListener('pointerleave', arm);
    node.addEventListener('focusin', () => window.clearTimeout(node._timer));
    node.addEventListener('focusout', arm);
    list.append(node);
    arm();
    const open = Array.from(list.children).filter(n => !n.hasAttribute('data-closing'));
    while (open.length > TOAST_LIMIT) {
      const victim = open.find(n => n.dataset.type !== 'error') || open[0];
      open.splice(open.indexOf(victim), 1);
      closeToast(victim);
    }
  }

  // ------------------------------------------------------------------ dialogs (native <dialog>, manual focus return)
  function openDialog(dialog, opener, fallback) {
    dialog._opener = opener || document.activeElement;
    dialog._fallback = fallback || null;
    dialog.showModal();
  }
  function prepareDialog(dialog, options = {}) {
    let pressedOnBackdrop = false;
    dialog.addEventListener('close', () => {
      const opener = dialog._opener;
      const target = opener && opener.isConnected ? opener : dialog._fallback;
      dialog._opener = null;
      if (target && target.isConnected) target.focus({preventScroll: true});
      if (options.onClose) options.onClose();
    });
    for (const button of $$('[data-dialog-close]', dialog)) button.addEventListener('click', () => dialog.close());
    if (options.backdropClose) {
      const outside = event => {
        const r = dialog.getBoundingClientRect();
        return event.clientX < r.left || event.clientX > r.right || event.clientY < r.top || event.clientY > r.bottom;
      };
      dialog.addEventListener('pointerdown', event => {
        pressedOnBackdrop = event.target === dialog && outside(event);
      });
      dialog.addEventListener('click', event => {
        if (event.target === dialog && pressedOnBackdrop && outside(event)) dialog.close();
        pressedOnBackdrop = false;
      });
    }
  }
  /** Shows a message in an alert of a dialog (toasts lie under the modal overlay, 00.md 7.2). */
  function showDialogAlert(alertId, message) {
    const alert = $('#' + alertId);
    $('.ui-alert-description', alert).textContent = message;
    alert.hidden = false;
    alert.scrollIntoView({block: 'nearest'});
  }
  const confirmDialog = $('#confirm-dialog');
  prepareDialog(confirmDialog, {backdropClose: true});
  /** Alert dialog with the consequence and a safe way out. action() runs on confirm; its error stays inside the dialog. */
  function confirmAction({title, description, confirmLabel, destructive, action, fallback}, opener) {
    const ok = $('#confirm-ok');
    $('#confirm-title').textContent = title;
    $('#confirm-description').textContent = description;
    $('#confirm-error').hidden = true;
    ok.textContent = confirmLabel;
    if (destructive) ok.dataset.variant = 'destructive';
    else delete ok.dataset.variant;
    ok.onclick = async () => {
      $('#confirm-error').hidden = true;
      setBusy(ok, true);
      try {
        await action();
        confirmDialog.close();
      } catch (error) {
        if (error && error.handled) confirmDialog.close();
        else showDialogAlert('confirm-error', messageOf(error));
      } finally {
        setBusy(ok, false);
      }
    };
    openDialog(confirmDialog, opener, fallback);
    $('#confirm-cancel').focus();
  }

  // ------------------------------------------------------------------ form validation (Russian messages, aria-invalid, aria-describedby)
  function clearFieldError(input) {
    const wrap = input.closest('.ui-field');
    const error = wrap && $('.ui-error', wrap);
    if (error) error.remove();
    if (wrap) delete wrap.dataset.invalid;
    input.removeAttribute('aria-invalid');
    if (input.dataset.describedby !== undefined) {
      if (input.dataset.describedby) input.setAttribute('aria-describedby', input.dataset.describedby);
      else input.removeAttribute('aria-describedby');
    }
  }
  function setFieldError(input, message) {
    const wrap = input.closest('.ui-field');
    clearFieldError(input);
    if (!wrap) return;
    if (input.dataset.describedby === undefined) input.dataset.describedby = input.getAttribute('aria-describedby') || '';
    const error = el('p', {class: 'ui-error', id: input.id + '-error', text: message});
    wrap.append(error);
    wrap.dataset.invalid = '';
    input.setAttribute('aria-invalid', 'true');
    input.setAttribute('aria-describedby', (input.dataset.describedby + ' ' + error.id).trim());
  }
  function validationMessage(input) {
    const v = input.validity;
    if (v.valueMissing) return 'Заполните поле.';
    if (v.typeMismatch) return input.type === 'url' ? 'Укажите адрес целиком, например https://example.com.' : 'Проверьте формат значения.';
    if (v.tooShort) return t('Не короче ' + input.minLength + ' символов.');
    if (v.tooLong) return t('Не длиннее ' + input.maxLength + ' символов.');
    if (v.rangeUnderflow) return t('Значение не меньше ' + input.min + '.');
    if (v.rangeOverflow) return t('Значение не больше ' + input.max + '.');
    if (v.badInput) return 'Введите число.';
    return 'Проверьте значение.';
  }
  /** Marks every invalid field of a form and returns the first one (or null). */
  function validateForm(form) {
    let first = null;
    for (const input of Array.from(form.elements)) {
      if (!(input instanceof HTMLInputElement || input instanceof HTMLSelectElement || input instanceof HTMLTextAreaElement)) continue;
      if (input.type === 'hidden' || input.disabled || input.closest('[hidden]')) continue;
      // tooShort is only reported for values typed by the user; a password manager fills the field from script.
      const short = input.minLength > 0 && input.value.length > 0 && input.value.length < input.minLength;
      if (input.validity.valid && !short) continue;
      setFieldError(input, short ? t('Не короче ' + input.minLength + ' символов.') : validationMessage(input));
      first = first || input;
    }
    return first;
  }
  function watchFieldErrors(form) {
    form.addEventListener('input', event => {
      if (event.target instanceof HTMLElement && event.target.getAttribute('aria-invalid') === 'true') clearFieldError(event.target);
    });
  }
  function clearFormErrors(form) {
    for (const input of $$('[aria-invalid]', form)) clearFieldError(input);
  }

  // ------------------------------------------------------------------ screens
  function setScreen(name) {
    if (name !== 'shell') livePlayer.stop();
    screen = name;
    for (const key of Object.keys(screens)) screens[key].hidden = key !== name;
    if (name === 'pending' || name === 'fatal') document.title = 'Dozor';
    if (name === 'login') document.title = 'Вход — Dozor';
  }
  function showFatal() {
    setScreen('fatal');
  }

  const loginForm = $('#login-form');
  function showLoginAlert(message, kind) {
    const alert = $('#login-alert');
    alert.dataset.variant = kind === 'error' ? 'destructive' : 'info';
    alert.setAttribute('role', kind === 'error' ? 'alert' : 'status');
    $('use', alert).setAttribute('href', kind === 'error' ? '#i-circle-alert' : '#i-info');
    $('#login-alert-text').textContent = message;
    alert.hidden = false;
  }
  function clearLogin() {
    $('#login-alert').hidden = true;
    clearFormErrors(loginForm);
  }
  /** Back to the login card: a logout, an expired session, or a first run (setup). */
  function showLogin({setup = false, notice = ''} = {}) {
    stopPlayer();
    for (const dialog of $$('dialog[open]')) dialog.close();
    csrf = '';
    settings = null;
    snapshot = null;
    lastOk = 0;
    failures = 0;
    currentTab = null;
    selectedLiveCamera = '';
    resetEvents();
    resetPlayer();
    $('#discovery-card').hidden = true;
    clearLogin();
    setScreen('login');
    $('#setup-field').hidden = !setup;
    setText($('#login-description'), setup ? t('Создайте пароль администратора и введите код первого запуска.') : t('Войдите в Dozor, чтобы открыть записи.'));
    field(loginForm, 'password').autocomplete = setup ? 'new-password' : 'current-password';
    if (notice) showLoginAlert(notice, 'info');
    const target = setup ? field(loginForm, 'token') : field(loginForm, 'password');
    target.focus({preventScroll: true});
  }
  function showLoginError(error) {
    const message = messageOf(error);
    showLoginAlert(message, 'error');
    const token = field(loginForm, 'token');
    const password = field(loginForm, 'password');
    const status = error && error.status;
    // The alert says what is wrong; the field only gets aria-invalid and points at the alert (no second message).
    const mark = input => {
      if (input.dataset.describedby === undefined) input.dataset.describedby = input.getAttribute('aria-describedby') || '';
      input.setAttribute('aria-invalid', 'true');
      input.setAttribute('aria-describedby', (input.dataset.describedby + ' login-alert-text').trim());
    };
    if (/код/i.test(message) && !$('#setup-field').hidden) mark(token);
    else if (status === 400 || status === 401) mark(password);
  }
  watchFieldErrors(loginForm);
  loginForm.addEventListener(
    'submit',
    run(
      async () => {
        clearLogin();
        const password = field(loginForm, 'password');
        const problem = validateForm(loginForm);
        if (problem) {
          problem.focus();
          return;
        }
        // The server counts bytes (bcrypt limit), the field counts characters: Cyrillic takes two bytes.
        if (new TextEncoder().encode(password.value).length > 72) {
          setFieldError(password, t('Не длиннее 72 байт: для кириллицы это до 36 символов.'));
          password.focus();
          return;
        }
        const auth = await api('/login', 'POST', {password: password.value, token: field(loginForm, 'token').value});
        csrf = auth.csrf;
        loginForm.reset();
        try {
          await enter();
        } catch (error) {
          if (!(error && error.handled)) showFatal();
        }
      },
      {button: $('#login-submit'), onError: showLoginError}
    )
  );

  // ------------------------------------------------------------------ router (hash: #overview, #live, #events, #cameras, #settings)
  // The menu (below) reads the same hash and marks the current item itself.
  function route() {
    let tab = location.hash.slice(1);
    if (!TABS.includes(tab)) {
      tab = 'overview';
      history.replaceState(null, '', '#overview');
    }
    showTab(tab);
  }
  function showTab(tab) {
    const changed = tab !== currentTab;
    currentTab = tab;
    for (const name of TABS) $('#tab-' + name).hidden = name !== tab;
    setText($('#page-title'), TITLES[tab]);
    document.title = TITLES[tab].replace(/ /g, ' ') + ' — Dozor';
    $('#storage-alert-action').hidden = tab === 'settings';
    $('#s3-alert-action').hidden = tab === 'settings';
    if (!changed) return;
    if (tab !== 'events') video.pause();
    if (tab !== 'live') livePlayer.stop();
    else renderLive();
    window.scrollTo(0, 0);
    if (tab === 'events') run(() => loadEvents())();
  }
  window.addEventListener('hashchange', () => {
    if (screen === 'shell') route();
  });

  // ------------------------------------------------------------------ entering the application
  async function enter() {
    const [loaded, status] = await Promise.all([api('/settings'), api('/status')]);
    settings = loaded;
    snapshot = status;
    lastOk = Date.now();
    failures = 0;
    buildFormatters();
    fillSettings();
    renderCameras();
    renderStatus();
    setScreen('shell');
    currentTab = null;
    route();
  }
  async function boot() {
    setScreen('pending');
    try {
      const auth = await fetchAuth();
      if (!auth.authenticated) {
        showLogin({setup: auth.setup_required});
        return;
      }
      csrf = auth.csrf;
      await enter();
    } catch (error) {
      if (!(error && error.handled)) showFatal();
    }
  }
  $('#retry').addEventListener('click', () => boot());
  // The hash is the router, so the skip link must not change it.
  $('#skip-link').addEventListener('click', event => {
    event.preventDefault();
    $('#main').focus();
  });
  // One handler for every logout button: the top bar's and the menu's (the busy button is the one clicked).
  const logout = run(async () => {
    await api('/logout', 'POST', {});
    showLogin();
  });
  for (const button of $$('[data-logout]')) button.addEventListener('click', logout);

  // ------------------------------------------------------------------ menu: the shared Sidebar of @jourloy/00 (vendor/a00-menu.js)
  // Left rail from 1024px, bottom bar below it. It marks the current item from the hash and follows hashchange itself;
  // the routing above stays here. The bundle is built by `make menu` (assets-src/menu), see docs/ui.md.
  // Every text of the Sidebar is passed from here, so that a change of its defaults upstream cannot alter the Russian text.
  const NAV_LABELS = {overview: 'Обзор', live: 'Онлайн', events: 'События', cameras: 'Камеры', settings: 'Настройки'};
  const MENU_LABELS = {
    collapse: 'Свернуть меню',
    pin: 'Закрепить меню',
    navigation: 'Разделы',
    brandTitle: 'Dozor',
    brandSigil: 'D',
    brandAriaLabel: 'Dozor',
  };
  // The app must work without the menu (a failed or blocked /vendor/a00-menu.js): then the top bar stays on wide screens
  // too, and its logout button is the way out (data-menu="missing", app.css).
  let menu = null;
  try {
    menu = window.DozorMenu.mount($('#menu'), {
      items: TABS.map(tab => ({hash: '#' + tab, label: NAV_LABELS[tab], icon: tab})),
      labels: MENU_LABELS,
      logoutLabel: 'Выйти',
      onLogout: logout,
    });
  } catch (error) {
    console.error('Dozor: the menu did not start, vendor/a00-menu.js is missing or broken', error);
    $('#shell').dataset.menu = 'missing';
  }

  // ------------------------------------------------------------------ overview
  const MOTION_LABELS = {auto: 'Сначала ONVIF, затем локальный детектор', local: 'Локальный детектор', onvif: 'Только ONVIF'};
  function setBadge(node, text, variant) {
    setText(node, text);
    if (node.dataset.variant !== variant) node.dataset.variant = variant;
  }
  function renderConnection() {
    const badgeNode = $('#system-badge');
    const note = $('#system-updated');
    if (failures >= 2 && lastOk) {
      // Repeated failures: keep the last snapshot and say so once, instead of a toast every 10 s (00.md 10.8).
      setBadge(badgeNode, 'Нет связи', 'warning');
      setText(note, t('Данные на ' + clock(lastOk)));
      note.hidden = false;
      return;
    }
    note.hidden = true;
    if (!snapshot) setBadge(badgeNode, 'Подключаемся…', 'secondary');
    else if (snapshot.disk_ready) setBadge(badgeNode, 'Система работает', 'success');
    else setBadge(badgeNode, 'Требуется настройка диска', 'warning');
  }
  function cameraBadge(camera) {
    if (!camera.enabled) return badge('Отключена', 'secondary');
    if (!camera.detector_healthy) return badge('Резервная непрерывная запись', 'warning');
    if (camera.motion) return badge('Есть движение', 'info');
    return badge('Наблюдение', 'success');
  }
  function streamText(camera) {
    const state = !camera.enabled ? 'Приём видео выключен' : camera.online ? 'Поток поступает' : 'Нет свежих фрагментов';
    return t(camera.last_segment > 0 ? state + ' · последний фрагмент ' + time(camera.last_segment) : state);
  }
  function emptyState({title, description, action, sticker, compact, bordered}) {
    const header = el('div', {class: 'ui-empty-header'});
    if (sticker) header.append(el('div', {class: 'ui-empty-media'}, el('span', {class: 'ui-icon-slot'}, el('img', {src: '/icons/' + sticker + '.webp', alt: '', width: '48', height: '48', decoding: 'async'}))));
    if (title) header.append(el('p', {class: 'ui-empty-title', text: t(title)}));
    if (description) header.append(el('p', {class: 'ui-empty-description', text: t(description)}));
    const box = el('div', {class: 'ui-empty', dataset: {compact: compact ? '' : undefined, bordered: bordered ? '' : undefined}}, header);
    if (action) box.append(el('div', {class: 'ui-empty-content'}, action));
    return box;
  }
  function renderStatus() {
    const s = snapshot;
    if (!s || !settings) return;
    const now = new Date();
    setText($('#today'), longDateFormat.format(now));
    $('#today').setAttribute('datetime', isoDateFormat.format(now));
    renderConnection();
    if (menu) menu.update({version: s.version || ''});
    setText($('#version-line'), s.version ? t('Версия Dozor ' + s.version) : 'Версия Dozor');

    // alerts
    const storage = $('#storage-alert');
    storage.hidden = Boolean(s.disk_ready);
    setText($('#storage-alert-text'), humanize(s.storage_error, t('Подключите и выберите диск в настройках.')));
    const upload = $('#s3-alert');
    upload.hidden = !(settings.s3.enabled && s.upload_error);
    setText($('#s3-alert-text'), humanize(s.upload_error, t('Часть записей не удалось выгрузить. Выгрузка повторится автоматически.')));

    // metrics
    const online = s.cameras.filter(c => c.online).length;
    const enabled = settings.cameras.filter(c => c.enabled).length;
    const cameras = $('#stat-cameras');
    renderIfChanged(cameras, [online, enabled], () => cameras.replaceChildren(number.format(online), el('span', {class: 'ui-metric-unit', text: t(' из ' + number.format(enabled))})));
    const total = Number(s.disk_total);
    const used = Number(s.disk_used);
    const meter = $('#disk-meter');
    if (total > 0) {
      const share = Math.min(1, Math.max(0, used / total));
      setText($('#stat-disk'), percent.format(share));
      setText($('#stat-disk-detail'), t(bytes(used) + ' из ' + bytes(total)));
      meter.style.setProperty('--meter-fill', share * 100 + '%');
      meter.setAttribute('aria-valuenow', String(Math.round(share * 100)));
      meter.setAttribute('aria-valuetext', percent.format(share) + ', ' + bytes(used) + ' из ' + bytes(total));
      meter.hidden = false;
    } else {
      setText($('#stat-disk'), '—');
      setText($('#stat-disk-detail'), 'ожидаем диск');
      meter.hidden = true;
    }
    setText($('#stat-queue'), number.format(Number(s.queue) || 0));
    setText($('#stat-queue-detail'), settings.s3.enabled ? t('части и описания событий') : t('выгрузка выключена'));

    // cameras
    const list = $('#camera-status');
    renderIfChanged(list, [s.cameras, settings.cameras.length, zone], () => {
      list.replaceChildren();
      if (!settings.cameras.length) {
        list.append(emptyState({description: t('Добавьте первую камеру — с поиска в сети или по RTSP-адресу.'), compact: true, action: el('a', {class: 'ui-button', dataset: {variant: 'outline', size: 'sm'}, href: '#cameras', text: t('Перейти к камерам')})}));
      } else if (!s.cameras.length) {
        list.append(emptyState({description: t('Состояние камер появится, когда подключится диск.'), compact: true}));
      } else {
        const rows = el('ul', {class: 'ui-item-list', dataset: {divided: ''}});
        for (const c of s.cameras) {
          rows.append(
            el('li', null, el('div', {class: 'ui-item status-row'}, el('div', {class: 'ui-item-content'}, el('p', {class: 'ui-item-title', text: c.name}), el('p', {class: 'ui-item-description status-time', text: streamText(c)})), el('div', {class: 'ui-item-actions'}, cameraBadge(c), c.enabled ? liveButton(c) : null)))
          );
        }
        list.append(rows);
      }
    });

    // journal
    const notices = $('#notices');
    renderIfChanged(notices, [s.notices, zone], () => {
      notices.replaceChildren();
      if (!s.notices.length) {
        notices.append(emptyState({description: t('Сообщений пока нет.'), compact: true}));
        return;
      }
      for (const n of s.notices) {
        const message = capitalize(t(String(n.message).replace(/:\s*[0-9a-f]{32}\s*$/i, '')));
        notices.append(el('div', {class: 'notice'}, el('time', {text: time(n.at), datetime: new Date(n.at).toISOString()}), el('span', {text: message})));
      }
    });
    renderUpdateStatus(s.update_status);
    renderLive();
  }

  // ------------------------------------------------------------------ live view
  let selectedLiveCamera = '';
  const liveVideo = $('#live-video');
  const LIVE_STATES = {
    connecting: ['Подключаемся…', 'secondary'],
    playing: ['В эфире', 'success'],
    paused: ['Пауза', 'secondary'],
    retrying: ['Переподключение', 'warning'],
    error: ['Недоступно', 'warning'],
  };
  function liveState(state, message) {
    const [label, variant] = LIVE_STATES[state];
    setBadge($('#live-badge'), label, variant);
    setText($('#live-message'), t(message));
    $('#live-frame').hidden = state === 'error' || state === 'retrying';
    $('#live-edge').disabled = state !== 'playing' && state !== 'paused';
  }
  const livePlayer = new window.DozorLivePlayer(liveVideo, liveState, () => showLogin({notice: MSG.sessionExpired}));
  function liveButton(camera) {
    return el('button', {
      type: 'button', class: 'ui-button', dataset: {variant: 'outline', size: 'sm'},
      'aria-label': 'Смотреть онлайн: ' + camera.name,
      on: {click: () => {
        selectedLiveCamera = camera.id;
        if (currentTab === 'live') renderLive();
        else location.hash = 'live';
      }},
    }, icon('live', {size: 16, slot: 'inline-start'}), 'Онлайн');
  }
  function renderLive(force = false) {
    if (!settings) return;
    const cameras = settings.cameras.filter(c => c.enabled);
    if (!cameras.some(c => c.id === selectedLiveCamera)) selectedLiveCamera = cameras.length ? cameras[0].id : '';
    const select = $('#live-camera');
    renderIfChanged(select, cameras.map(c => [c.id, c.name]), () => {
      select.replaceChildren();
      for (const camera of cameras) select.add(new Option(camera.name, camera.id));
    });
    select.value = selectedLiveCamera;
    const missing = !cameras.length || !snapshot || !snapshot.disk_ready;
    $('#live-empty').hidden = !missing;
    $('#live-content').hidden = missing;
    if (missing) {
      livePlayer.stop();
      const hasCameras = cameras.length > 0;
      $('#live-empty').replaceChildren(emptyState({
        title: hasCameras ? 'Видеослужба недоступна' : 'Нет включённых камер',
        description: hasCameras ? 'Подключите диск и дождитесь запуска камер. Онлайн-просмотр использует работающую видеослужбу.' : 'Добавьте камеру или включите существующую в разделе «Камеры».',
        sticker: 'dual-camera', bordered: true,
        action: el('a', {class: 'ui-button', href: hasCameras ? '#settings' : '#cameras', text: hasCameras ? 'Перейти к настройкам' : 'Перейти к камерам'}),
      }));
      return;
    }
    const camera = cameras.find(c => c.id === selectedLiveCamera);
    setText($('#live-title'), camera.name);
    liveVideo.setAttribute('aria-label', 'Прямая трансляция: ' + camera.name);
    if (screen !== 'shell' || currentTab !== 'live' || document.hidden) return;
    const url = '/api/v1/cameras/' + encodeURIComponent(camera.id) + '/live/index.m3u8';
    if (force || livePlayer.url !== url) livePlayer.start(url);
  }
  $('#live-camera').addEventListener('change', event => {
    selectedLiveCamera = event.target.value;
    renderLive();
  });
  $('#live-retry').addEventListener('click', () => renderLive(true));
  $('#live-edge').addEventListener('click', () => livePlayer.goLive());
  window.addEventListener('pagehide', () => livePlayer.stop());
  window.addEventListener('pageshow', () => {
    if (screen === 'shell') renderLive();
  });

  // ------------------------------------------------------------------ cameras
  function cameraCard(camera) {
    const title = 'camera-' + camera.id;
    const edit = el('button', {type: 'button', class: 'ui-button', dataset: {variant: 'outline', size: 'sm'}, text: 'Настроить'});
    edit.addEventListener('click', () => editCamera(camera, edit));
    const del = el('button', {type: 'button', class: 'ui-button', dataset: {variant: 'ghost', size: 'sm', tone: 'danger'}}, icon('trash', {size: 16, slot: 'inline-start'}), 'Удалить');
    del.addEventListener('click', () =>
      confirmAction(
        {
          title: t('Удалить камеру «' + camera.name + '»?'),
          description: t('Камера будет удалена из конфигурации. Существующие события останутся в архиве.'),
          confirmLabel: 'Удалить',
          destructive: true,
          action: async () => {
            await api('/cameras/' + camera.id, 'DELETE');
            settings = await api('/settings');
            renderCameras();
            toast(t('Камера удалена. Записи остались в архиве.'), 'success');
            refreshStatus().catch(() => {});
          },
          fallback: $('#add-camera'),
        },
        del
      )
    );
    return el(
      'article',
      {class: 'ui-card camera-card', 'aria-labelledby': title},
      el('div', {class: 'ui-card-header'}, el('h3', {class: 'ui-card-title', id: title, text: camera.name}), el('span', {class: 'ui-card-action'}, camera.enabled ? badge('Включена', 'success') : badge('Отключена', 'secondary'))),
      el('div', {class: 'ui-card-content'}, el('p', {class: 'camera-url', text: camera.url}), el('p', {class: 'camera-meta', text: t('Источник движения: ' + (MOTION_LABELS[camera.motion] || camera.motion))})),
      el('div', {class: 'ui-card-footer'}, camera.enabled ? liveButton(camera) : null, edit, del)
    );
  }
  function renderCameras() {
    const grid = $('#camera-list');
    grid.replaceChildren();
    const filter = $('#filter-camera');
    const selected = filter.value;
    filter.replaceChildren(new Option('Все камеры', ''));
    for (const camera of settings.cameras) {
      filter.add(new Option(camera.name, camera.id));
      grid.append(cameraCard(camera));
    }
    filter.value = settings.cameras.some(c => c.id === selected) ? selected : '';
    if (!settings.cameras.length) {
      grid.append(
        emptyState({
          sticker: 'dual-camera',
          title: 'Камер пока нет',
          description: t('Найдите камеры в сети или добавьте первую по RTSP-адресу.'),
          bordered: true,
          action: el('button', {type: 'button', class: 'ui-button', on: {click: ev => editCamera(null, ev.currentTarget)}}, icon('plus', {slot: 'inline-start'}), 'Добавить камеру'),
        })
      );
    }
    // The overview list depends on the configured cameras too.
    $('#camera-status')._signature = null;
    if (snapshot) renderStatus();
  }

  const cameraDialog = $('#camera-dialog');
  const cameraForm = $('#camera-form');
  prepareDialog(cameraDialog, {backdropClose: false});
  watchFieldErrors(cameraForm);
  function setSliderFill(input) {
    const min = Number(input.min) || 0;
    const max = Number(input.max) || 1;
    const share = max > min ? (Number(input.value) - min) / (max - min) : 0;
    input.style.setProperty('--p', String(Math.min(1, Math.max(0, share))));
  }
  function showSensitivity() {
    const input = field(cameraForm, 'sensitivity');
    const label = percent.format(Number(input.value));
    setText($('#cam-sensitivity-value'), label);
    input.setAttribute('aria-valuetext', label);
    setSliderFill(input);
  }
  field(cameraForm, 'sensitivity').addEventListener('input', showSensitivity);
  function resetCameraMessages() {
    $('#camera-error').hidden = true;
    $('#probe-result').hidden = true;
    $('#profiles').hidden = true;
    $('#profiles-list').replaceChildren();
    $('#camera-note').hidden = true;
    clearFormErrors(cameraForm);
  }
  const NEW_CAMERA = {id: '', name: '', url: '', sub_url: '', onvif: '', username: 'admin', motion: 'auto', sensitivity: 0.7, masks: [], enabled: true, has_password: false};
  function editCamera(camera, opener, preset) {
    const c = {...NEW_CAMERA, ...(camera || {}), ...(preset || {})};
    resetCameraMessages();
    setText($('#camera-dialog-title'), c.id ? 'Настройка камеры' : 'Новая камера');
    for (const key of ['id', 'name', 'url', 'sub_url', 'onvif', 'username', 'sensitivity']) field(cameraForm, key).value = c[key] ?? '';
    field(cameraForm, 'motion').value = c.motion || 'auto';
    field(cameraForm, 'password').value = '';
    setText($('#cam-password-hint'), c.has_password ? t('Сохранён — оставьте пустым, чтобы не менять.') : t('Оставьте пустым, если камера без пароля.'));
    field(cameraForm, 'masks').value = JSON.stringify(c.masks || []);
    field(cameraForm, 'enabled').checked = Boolean(c.enabled);
    showSensitivity();
    openDialog(cameraDialog, opener, $('#add-camera'));
    field(cameraForm, 'name').focus();
  }
  /** The body of PUT /cameras/{id}, POST /cameras/probe and POST /profiles. The fields are exactly those the server accepts. */
  function cameraData() {
    const data = {};
    for (const key of ['id', 'name', 'url', 'sub_url', 'onvif', 'username', 'password', 'motion']) {
      data[key] = key === 'password' ? field(cameraForm, key).value : field(cameraForm, key).value.trim();
    }
    data.sensitivity = Number(field(cameraForm, 'sensitivity').value);
    data.enabled = field(cameraForm, 'enabled').checked;
    const masks = field(cameraForm, 'masks');
    try {
      data.masks = JSON.parse(masks.value);
    } catch {
      setFieldError(masks, t('Проверьте JSON исключаемых областей.'));
      masks.focus();
      throw new UserError(t('Проверьте JSON исключаемых областей.'));
    }
    return data;
  }
  const showCameraError = error => {
    // A message already shown at its field (the masks) is not repeated in the alert.
    if (error instanceof UserError) return;
    showDialogAlert('camera-error', messageOf(error));
  };
  $('#add-camera').addEventListener('click', ev => editCamera(null, ev.currentTarget));
  cameraForm.addEventListener(
    'submit',
    run(
      async () => {
        resetCameraMessages();
        const problem = validateForm(cameraForm);
        // The server counts bytes, the field counts characters: Cyrillic takes two bytes.
        const name = field(cameraForm, 'name');
        let invalid = problem;
        if (new TextEncoder().encode(name.value.trim()).length > 100) {
          setFieldError(name, t('Не длиннее 100 байт: для кириллицы это до 50 символов.'));
          invalid = invalid || name;
        }
        if (invalid) {
          invalid.focus();
          return;
        }
        const camera = cameraData();
        // PUT answers with the saved settings (the same body as GET /settings): no second request that could fail after the save.
        settings = await api('/cameras/' + (camera.id || 'new'), 'PUT', camera);
        renderCameras();
        cameraDialog.close();
        toast(camera.enabled ? t('Камера сохранена. Подключаем потоки.') : t('Камера сохранена и выключена.'), 'success');
        refreshStatus().catch(() => {});
      },
      {button: $('#camera-submit'), onError: showCameraError}
    )
  );
  $('#probe-camera').addEventListener(
    'click',
    run(
      async () => {
        resetCameraMessages();
        const probe = await api('/cameras/probe', 'POST', cameraData(), {fallback: t('Поток не открылся. Проверьте адрес, логин и пароль камеры.')});
        const alert = $('#probe-result');
        const compatible = Boolean(probe.browser_compatible);
        alert.dataset.variant = compatible ? 'success' : 'warning';
        $('use', alert).setAttribute('href', compatible ? '#i-circle-check' : '#i-alert');
        setText($('#probe-result-title'), 'Поток доступен');
        setText(
          $('#probe-result-text'),
          t([plain.format(probe.width) + ' × ' + plain.format(probe.height), probe.video, 'звук: ' + (probe.audio || 'нет'), compatible ? 'подходит для браузера' : 'просмотр зависит от поддержки кодеков браузером'].join(' · '))
        );
        alert.hidden = false;
        alert.scrollIntoView({block: 'nearest'});
      },
      {button: $('#probe-camera'), onError: showCameraError}
    )
  );
  $('#get-profiles').addEventListener(
    'click',
    run(
      async () => {
        resetCameraMessages();
        const profiles = await api('/profiles', 'POST', cameraData(), {fallback: t('Профили не получены. Проверьте адрес ONVIF, логин и пароль камеры.')});
        const list = $('#profiles-list');
        list.replaceChildren();
        if (!profiles.length) {
          list.append(el('li', null, el('p', {class: 'ui-hint', text: t('Профили не найдены. Укажите RTSP-адрес вручную.')})));
        }
        for (const profile of profiles) {
          const main = el('button', {type: 'button', class: 'ui-button', dataset: {variant: 'outline', size: 'sm'}, text: 'Основной поток'});
          main.addEventListener('click', () => {
            field(cameraForm, 'url').value = profile.url;
            clearFieldError(field(cameraForm, 'url'));
            setText($('#camera-note'), t('Адрес основного потока подставлен из профиля «' + profile.name + '».'));
            $('#camera-note').hidden = false;
          });
          const sub = el('button', {type: 'button', class: 'ui-button', dataset: {variant: 'outline', size: 'sm'}, text: t('Для детектора')});
          sub.addEventListener('click', () => {
            field(cameraForm, 'sub_url').value = profile.url;
            setText($('#camera-note'), t('Адрес для детектора подставлен из профиля «' + profile.name + '».'));
            $('#camera-note').hidden = false;
          });
          list.append(
            el('li', null, el('div', {class: 'ui-item profile-row', dataset: {variant: 'outline'}}, el('div', {class: 'ui-item-content'}, el('p', {class: 'ui-item-title', text: profile.name}), el('p', {class: 'code-text', text: profile.url})), el('div', {class: 'ui-item-actions'}, main, sub)))
          );
        }
        $('#profiles').hidden = false;
        $('#profiles').scrollIntoView({block: 'nearest'});
      },
      {button: $('#get-profiles'), onError: showCameraError}
    )
  );

  function hostOf(endpoint) {
    try {
      return new URL(endpoint).hostname;
    } catch {
      return endpoint;
    }
  }
  /** The model name from the ONVIF scopes, when the camera announced one. */
  function modelOf(scopes) {
    const match = typeof scopes === 'string' ? scopes.match(/onvif:\/\/www\.onvif\.org\/name\/(\S+)/) : null;
    if (!match) return '';
    try {
      return decodeURIComponent(match[1]).replace(/_/g, ' ');
    } catch {
      return '';
    }
  }
  $('#discover').addEventListener(
    'click',
    run(
      async () => {
        const card = $('#discovery-card');
        const target = $('#discovery-results');
        const summary = $('#discovery-summary');
        summary.hidden = true;
        card.hidden = false;
        target.replaceChildren(el('p', {class: 'ui-loading', role: 'status'}, spinner(), 'Ищем камеры в сети…'));
        let found;
        try {
          found = await api('/discovery', 'POST', {}, {fallback: t('Поиск не удался. Повторите попытку или добавьте камеру вручную.')});
        } catch (error) {
          if (error && error.handled) return;
          target.replaceChildren(el('div', {class: 'ui-alert', dataset: {variant: 'destructive'}, role: 'alert'}, icon('circle-alert'), el('p', {class: 'ui-alert-description', text: messageOf(error)})));
          return;
        }
        target.replaceChildren();
        if (!found.length) {
          target.append(emptyState({description: t('Камеры не ответили. ONVIF может быть выключен; используйте ручное добавление.'), compact: true}));
          announce('Камеры в сети не найдены');
          return;
        }
        summary.textContent = 'Найдено: ' + number.format(found.length) + ' ' + pluralize(found.length, ['камера', 'камеры', 'камер']);
        summary.hidden = false;
        announce(summary.textContent);
        const rows = el('ul', {class: 'ui-item-list', dataset: {divided: ''}});
        for (const camera of found) {
          const host = hostOf(camera.endpoint);
          const model = modelOf(camera.scopes);
          const add = el('button', {type: 'button', class: 'ui-button', dataset: {variant: 'outline', size: 'sm'}, 'aria-label': 'Добавить камеру ' + host}, icon('plus', {size: 16, slot: 'inline-start'}), 'Добавить');
          add.addEventListener('click', () => editCamera(null, add, {onvif: camera.endpoint, name: 'Камера ' + host}));
          rows.append(el('li', null, el('div', {class: 'ui-item discovery-row'}, el('div', {class: 'ui-item-content'}, el('p', {class: 'ui-item-title', text: model ? host + ' · ' + model : host}), el('p', {class: 'code-text', text: camera.endpoint})), el('div', {class: 'ui-item-actions'}, add))));
        }
        target.append(rows);
      },
      {button: $('#discover')}
    )
  );

  // ------------------------------------------------------------------ events and the player
  const eventsState = {items: [], offset: 0, more: false, selectedId: null, loadToken: 0, openToken: 0, error: '', unavailable: false, refocus: false};
  let eventParts = [];
  let partIndex = 0;
  let timelineDragging = false;
  const video = $('#video');
  const timeline = $('#timeline');
  const nameOf = id => {
    const camera = settings && settings.cameras.find(c => c.id === id);
    return camera ? camera.name : 'Удалённая камера';
  };
  const EVENT_STATUS = e => (e.status !== 'closed' ? ['Записывается…', 'info'] : e.lost ? ['Есть потери до загрузки', 'destructive'] : e.uploaded ? ['Копия в S3 подтверждена', 'success'] : ['Ожидает выгрузки', 'secondary']);

  function resetEvents() {
    eventsState.items = [];
    eventsState.offset = 0;
    eventsState.more = false;
    eventsState.selectedId = null;
    eventsState.error = '';
    eventsState.unavailable = false;
    eventsState.refocus = false;
    eventsState.loadToken++;
    eventsState.openToken++;
    $('#event-list').replaceChildren();
  }
  /** A busy button is disabled and loses focus, and the rebuilt list removes it: remember that the focus was on it (see renderEvents). */
  const fromList = handler => event => {
    if (document.activeElement === event.currentTarget) eventsState.refocus = true;
    return handler(event);
  };
  /** Rebuilds the list. A rebuild removes the focused button, so focus that was inside moves to the new row or button (keyboard users keep their place). */
  function renderEvents(loading, firstNew = 0) {
    const box = $('#event-list');
    if (box.contains(document.activeElement)) eventsState.refocus = true;
    box.setAttribute('aria-busy', loading ? 'true' : 'false');
    const keep = $('.event-scroll', box);
    const scrollTop = keep ? keep.scrollTop : 0;
    box.replaceChildren();
    const settle = target => {
      const lost = document.activeElement === document.body || box.contains(document.activeElement);
      if (eventsState.refocus && target && lost) target.focus();
      eventsState.refocus = false;
    };
    // No disk is not a failure to retry: say what is missing and where to fix it (00.md section 10.7).
    const unavailable = () => {
      const link = el('a', {class: 'ui-button', dataset: {variant: 'outline', size: 'sm'}, href: '#settings', text: 'Выбрать диск'});
      box.append(emptyState({description: 'Архив недоступен, пока не подключён диск.', compact: true, action: link}));
      settle(link);
    };
    if (loading) {
      box.append(el('div', {class: 'event-skeletons', role: 'status', 'aria-label': 'Загрузка событий'}, el('div', {class: 'ui-skeleton event-skeleton'}), el('div', {class: 'ui-skeleton event-skeleton'}), el('div', {class: 'ui-skeleton event-skeleton'})));
      return;
    }
    if (eventsState.error && eventsState.unavailable) {
      unavailable();
      return;
    }
    if (eventsState.error) {
      const retry = el('button', {type: 'button', class: 'ui-button', dataset: {variant: 'outline', size: 'sm'}, on: {click: fromList(run(() => loadEvents()))}}, 'Повторить');
      box.append(emptyState({title: 'Не удалось загрузить события', description: eventsState.error, compact: true, action: retry}));
      settle(retry);
      return;
    }
    if (!eventsState.items.length) {
      if (snapshot && snapshot.disk_ready === false) {
        unavailable();
        return;
      }
      box.append(emptyState({description: 'За выбранный период событий пока нет.', compact: true}));
      settle(null);
      return;
    }
    const rows = el('ul', {class: 'ui-item-list event-scroll', 'aria-label': 'События'});
    for (const event of eventsState.items) {
      const [label, variant] = EVENT_STATUS(event);
      const row = el(
        'button',
        {type: 'button', class: 'ui-item event-row', dataset: {id: event.id}, 'aria-current': event.id === eventsState.selectedId ? 'true' : null},
        el('span', {class: 'ui-item-content'}, el('span', {class: 'ui-item-title', text: nameOf(event.camera_id)}), el('span', {class: 'ui-item-description event-time', text: time(event.start)})),
        badge(label, variant)
      );
      row.addEventListener('click', run(() => openEvent(event.id), {button: row}));
      rows.append(el('li', null, row));
    }
    box.append(rows);
    rows.scrollTop = scrollTop;
    if (eventsState.more) {
      box.append(el('div', {class: 'event-more'}, el('button', {type: 'button', class: 'ui-button', dataset: {variant: 'outline', size: 'sm'}, text: 'Загрузить ещё', on: {click: fromList(run(() => loadEvents(true)))}})));
    }
    settle($$('.event-row', rows)[firstNew] || $('.event-row', rows));
  }
  async function loadEvents(append) {
    append = append === true;
    if (!settings) return;
    const token = ++eventsState.loadToken;
    if (!append) {
      eventsState.offset = 0;
      eventsState.error = '';
      eventsState.unavailable = false;
      if (!eventsState.items.length) renderEvents(true);
    }
    const query = new URLSearchParams({offset: String(eventsState.offset)});
    if ($('#filter-camera').value) query.set('camera', $('#filter-camera').value);
    if ($('#filter-date').value) query.set('date', $('#filter-date').value);
    let page;
    try {
      page = await api('/events?' + query, 'GET', undefined, {fallback: MSG.eventsFailed});
    } catch (error) {
      if (token !== eventsState.loadToken || (error && error.handled)) return;
      if (!append) eventsState.items = [];
      // The archive answers 503 while no disk is mounted (the status says the same with disk_ready).
      const status = (error && error.status) || 0;
      eventsState.unavailable = status === 503 || (status > 0 && Boolean(snapshot && snapshot.disk_ready === false));
      eventsState.error = messageOf(error);
      eventsState.more = false;
      renderEvents(false);
      return;
    }
    if (token !== eventsState.loadToken) return;
    const firstNew = append ? eventsState.items.length : 0;
    eventsState.items = append ? eventsState.items.concat(page) : page;
    eventsState.offset += page.length;
    eventsState.more = page.length === 200;
    renderEvents(false, firstNew);
    if (!append) announce(eventsState.items.length ? 'Событий в списке: ' + number.format(eventsState.items.length) + (eventsState.more ? ' и больше' : '') : 'Событий нет');
  }

  function stopPlayer() {
    video.pause();
    video.removeAttribute('src');
    video.onloadedmetadata = null;
    video.load();
  }
  function resetPlayer() {
    eventParts = [];
    partIndex = 0;
    setText($('#event-title'), 'Выберите событие');
    $('#event-info').hidden = true;
    $('#player-body').hidden = true;
    $('#player-empty').hidden = false;
  }
  function updateTimeline(value) {
    timeline.value = String(value);
    setSliderFill(timeline);
    const label = time(Number(value));
    setText($('#timeline-time'), label);
    timeline.setAttribute('aria-valuetext', label);
  }
  function describeEvent(event) {
    const info = $('#event-info');
    const flags = [];
    if (event.short_prebuffer) flags.push('неполная предыстория');
    if (event.incomplete) flags.push('есть пропуски');
    if (event.lost) flags.push('часть записей удалена до выгрузки');
    info.replaceChildren(el('p', {text: time(event.start) + ' — ' + time(event.end)}));
    if (flags.length) info.append(el('p', {text: capitalize(t(flags.join(' · ')))}));
    info.hidden = false;
  }
  async function openEvent(id) {
    const token = ++eventsState.openToken;
    eventsState.selectedId = id;
    for (const row of $$('.event-row')) {
      if (row.dataset.id === id) row.setAttribute('aria-current', 'true');
      else row.removeAttribute('aria-current');
    }
    const {event, parts} = await api('/events/' + id);
    if (token !== eventsState.openToken) return;
    setText($('#event-title'), nameOf(event.camera_id));
    describeEvent(event);
    $('#player-empty').hidden = true;
    $('#player-body').hidden = false;
    eventParts = parts.filter(p => !p.deleted);
    const has = eventParts.length > 0;
    $('#video-frame').hidden = !has;
    $('#parts-empty').hidden = has;
    $('#timeline-field').hidden = !has;
    $('#parts-field').hidden = !has;
    const partsBox = $('#parts');
    partsBox.replaceChildren();
    if (has) {
      timeline.min = String(eventParts[0].start);
      timeline.max = String(eventParts[eventParts.length - 1].end);
      for (const [i, part] of eventParts.entries()) {
        const toggle = el('button', {type: 'button', class: 'ui-toggle', 'aria-pressed': 'false', text: clock(part.start)});
        toggle.addEventListener('click', () => playPart(i));
        partsBox.append(toggle);
      }
      playPart(0);
    } else {
      stopPlayer();
      $('#download').hidden = true;
    }
    if (window.matchMedia('(max-width: 1023px)').matches) $('#player-body').closest('.player-card').scrollIntoView({block: 'start', behavior: reducedMotion.matches ? 'auto' : 'smooth'});
  }
  function playPart(index, offset = 0) {
    partIndex = index;
    const part = eventParts[index];
    updateTimeline(part.start);
    video.onloadedmetadata = () => {
      video.currentTime = offset;
    };
    video.src = '/api/v1/parts/' + encodeURIComponent(part.id) + '/video';
    $('#download').href = video.getAttribute('src') + '?download=1';
    $('#download').hidden = false;
    $$('#parts .ui-toggle').forEach((toggle, i) => toggle.setAttribute('aria-pressed', i === index ? 'true' : 'false'));
  }
  video.addEventListener('error', () => {
    if (video.getAttribute('src')) toast(t('Видео недоступно или кодек не поддерживается браузером. Проверьте наличие записи и попробуйте скачать часть.'), 'error', 'video');
  });
  video.addEventListener('ended', () => {
    if (partIndex + 1 < eventParts.length) {
      playPart(partIndex + 1);
      video.play().catch(() => {});
    }
  });
  /** Maps the concatenated media time of a part to wall clock time, preserving the known gaps. */
  function clockTime(part, seconds) {
    let value = part.start + seconds * 1000;
    for (const gap of part.gaps || []) {
      if (gap.start >= part.start && value >= gap.start) value += gap.end - gap.start;
    }
    return Math.min(value, part.end);
  }
  video.addEventListener('timeupdate', () => {
    const part = eventParts[partIndex];
    if (!part || timelineDragging) return;
    updateTimeline(clockTime(part, video.currentTime));
  });
  const endDrag = () => window.setTimeout(() => {
    timelineDragging = false;
  }, 0);
  timeline.addEventListener('pointerdown', () => {
    timelineDragging = true;
  });
  timeline.addEventListener('pointerup', endDrag);
  timeline.addEventListener('pointercancel', endDrag);
  timeline.addEventListener('input', () => {
    timelineDragging = true;
    updateTimeline(Number(timeline.value));
  });
  timeline.addEventListener('change', () => {
    timelineDragging = false;
    const target = Number(timeline.value);
    const i = eventParts.findIndex(p => target >= p.start && target <= p.end);
    if (i < 0) {
      toast(t('В выбранное время нет локальной записи.'), 'warning', 'seek');
      return;
    }
    const part = eventParts[i];
    let offset = target - part.start;
    for (const gap of part.gaps || []) {
      if (target >= gap.start && target < gap.end) {
        toast(t('В выбранное время был разрыв потока.'), 'warning', 'seek');
        return;
      }
      if (gap.start >= part.start && gap.end <= target) offset -= gap.end - gap.start;
    }
    if (i === partIndex) video.currentTime = offset / 1000;
    else playPart(i, offset / 1000);
  });
  $('#refresh-events').addEventListener('click', run(() => loadEvents()));
  $('#filter-camera').addEventListener('change', run(() => loadEvents()));
  $('#filter-date').addEventListener('change', run(() => loadEvents()));

  // ------------------------------------------------------------------ settings
  const settingsForm = $('#settings-form');
  watchFieldErrors(settingsForm);
  function renderDiskCurrent() {
    const box = $('#disk-current');
    box.replaceChildren();
    if (settings && settings.disk_uuid) box.append('Сейчас выбран раздел: ', el('code', {text: settings.disk_uuid}));
    else box.append(t('Диск пока не выбран.'));
  }
  function fillSettings() {
    const form = settingsForm;
    const s3 = settings.s3;
    for (const key of ['endpoint', 'region', 'bucket', 'prefix']) field(form, key).value = s3[key] || '';
    field(form, 's3_enabled').checked = Boolean(s3.enabled);
    field(form, 'path_style').checked = Boolean(s3.path_style);
    field(form, 'rate').value = String(s3.bytes_per_second / 1024);
    field(form, 'timezone').value = settings.timezone;
    field(form, 'auto_update').checked = Boolean(settings.auto_update);
    field(form, 'release_url').value = settings.release_url || '';
    field(form, 'access_key').value = '';
    field(form, 'secret_key').value = '';
    setText($('#set-keys-hint'), settings.has_s3_credentials ? t('Ключи сохранены — оставьте поля пустыми, чтобы не менять.') : t('Введите ключи доступа к бакету.'));
    clearFormErrors(form);
    renderDiskCurrent();
  }
  const showSettingsError = error => {
    setText($('#settings-error-text'), messageOf(error));
    $('#settings-error').hidden = false;
  };
  settingsForm.addEventListener(
    'submit',
    run(
      async () => {
        $('#settings-error').hidden = true;
        const form = settingsForm;
        const problem = validateForm(form);
        const zoneField = field(form, 'timezone');
        let invalid = problem;
        if (!zoneField.validity.valueMissing) {
          try {
            new Intl.DateTimeFormat('ru-RU', {timeZone: zoneField.value.trim()});
          } catch {
            setFieldError(zoneField, 'Неизвестный часовой пояс. Пример: Europe/Moscow.');
            invalid = invalid || zoneField;
          }
        }
        if (invalid) {
          invalid.focus();
          return;
        }
        const s3 = {};
        for (const key of ['endpoint', 'region', 'bucket', 'prefix', 'access_key', 'secret_key']) s3[key] = field(form, key).value.trim();
        s3.enabled = field(form, 's3_enabled').checked;
        s3.path_style = field(form, 'path_style').checked;
        s3.bytes_per_second = Math.round(Number(field(form, 'rate').value) * 1024);
        settings = await api('/settings', 'PUT', {timezone: field(form, 'timezone').value.trim(), s3, auto_update: field(form, 'auto_update').checked, release_url: field(form, 'release_url').value.trim()});
        buildFormatters();
        fillSettings();
        renderCameras();
        toast(t('Настройки сохранены. Службы переподключаются.'), 'success');
        refreshStatus().catch(() => {});
      },
      {button: $('#settings-submit'), onError: showSettingsError}
    )
  );

  /** Human-readable state of the automatic update from the text the updater writes (update-status.json). */
  function describeUpdate(raw) {
    const text = String(raw || '').trim();
    if (!text) return null;
    const installed = text.match(/^Установлена\s+(v?\d+\.\d+\.\d+)\s+·\s+(\S+)$/);
    if (installed) {
      const at = new Date(installed[2]);
      const when = Number.isNaN(at.getTime()) ? '' : ' · обновлено ' + fullFormat.format(at);
      return {tone: 'success', text: t('Установлена версия ' + installed[1].replace(/^v/, '') + when)};
    }
    const known = [
      [/^Установлена актуальная версия$/, 'success', 'Установлена актуальная версия.'],
      [/^Автообновление выключено/, 'neutral', 'Автообновление выключено.'],
      [/^Не установлен ключ проверки/, 'warning', 'Не установлен ключ проверки подписи. Его устанавливает администратор на устройстве.'],
      [/^Обновление загружено/, 'info', 'Обновление загружено. Установка начнётся, когда завершатся текущие события.'],
      [/^Новый пакет несовместим/, 'warning', 'Новый пакет обновления не подходит для этого устройства.'],
      [/^неверная подпись/, 'warning', 'Подпись обновления не прошла проверку. Обновление не установлено.'],
      [/^недопустимый пакет/, 'warning', 'Пакет обновления отклонён. Обновление не установлено.'],
      [/^обновления требуют HTTPS/, 'warning', 'Для обновлений нужен адрес с HTTPS.'],
      [/^обновление не прошло проверку/, 'warning', 'Новая версия не прошла проверку после установки. Восстановлена предыдущая версия.'],
      [/^неверный SHA256/, 'warning', 'Контрольная сумма пакета не совпала. Обновление не установлено.'],
      [/^(не удалось скачать пакет|пакет недоступен|пакет повреждён)/, 'warning', 'Пакет обновления не скачан или повреждён. Проверка повторится позже.'],
      [/^версия уже существует с другим хешем/, 'warning', 'Пакет с этой версией отличается от уже скачанного. Обновление не установлено.'],
      [/^версия (подписанного пакета|бинарника) не совпадает/, 'warning', 'Версии релиза и пакета не совпадают. Обновление не установлено.'],
      [/^(Dozor недоступна|не удалось определить версию работающей Dozor)/, 'warning', 'Не удалось получить версию работающего приложения. Обновление отложено до следующей проверки.'],
      [/^источник обновлений/, 'warning', 'Источник обновлений недоступен. Проверка повторится позже.'],
    ];
    for (const [pattern, tone, message] of known) if (pattern.test(text)) return {tone, text: t(message)};
    return {tone: 'neutral', text: t('Не удалось определить результат последней проверки обновлений. Проверка повторится позже.')};
  }
  const UPDATE_TONES = {success: ['success', 'circle-check'], warning: ['warning', 'alert'], info: ['info', 'info'], neutral: ['default', 'info']};
  function renderUpdateStatus(raw) {
    const box = $('#update-status');
    const state = describeUpdate(raw);
    if (!state) {
      box.hidden = true;
      return;
    }
    const [variant, symbol] = UPDATE_TONES[state.tone];
    box.dataset.variant = variant;
    $('use', box).setAttribute('href', '#i-' + symbol);
    setText($('#update-status-text'), state.text);
    box.hidden = false;
  }

  const diskBox = $('#disk-list');
  async function loadDisks() {
    diskBox.replaceChildren(el('p', {class: 'ui-loading', role: 'status'}, spinner(), 'Читаем список дисков…'));
    let result;
    try {
      result = await api('/disks');
    } catch (error) {
      diskBox.replaceChildren();
      throw error;
    }
    diskBox.replaceChildren();
    if (result.development) {
      diskBox.append(el('div', {class: 'ui-alert', dataset: {variant: 'info'}}, icon('info'), el('p', {class: 'ui-alert-description', text: t('Режим разработки: используется явно указанный каталог. Выбор физического диска доступен на Raspberry Pi.')})));
      return;
    }
    const selected = result.selected_uuid || (settings && settings.disk_uuid) || '';
    const rows = el('ul', {class: 'ui-item-list', dataset: {divided: ''}});
    const addDisk = (disk, depth) => {
      const meta = el('p', {class: 'ui-item-description disk-meta'});
      meta.append(el('span', {text: bytes(Number(disk.size))}), el('span', {text: disk.fstype || t('без файловой системы')}), disk.uuid ? el('code', {text: disk.uuid}) : el('span', {text: 'нет UUID'}), el('span', {text: disk.fsavail == null ? t('свободное место — после монтирования') : 'свободно ' + bytes(Number(disk.fsavail))}));
      const row = el('div', {class: 'ui-item disk-row'}, el('div', {class: 'ui-item-content'}, el('p', {class: 'ui-item-title', text: disk.model || disk.name}), meta));
      row.style.setProperty('--depth', String(depth));
      if (disk.uuid && ['ext4', 'exfat'].includes(disk.fstype)) {
        const chosen = disk.uuid === selected;
        if (chosen) {
          // The state in use is a status (a word), not a disabled button: a disabled label would fall below 4.5:1.
          row.append(el('div', {class: 'ui-item-actions'}, badge('Выбран', 'success')));
        } else {
          const button = el('button', {type: 'button', class: 'ui-button', dataset: {variant: 'outline', size: 'sm'}, text: 'Использовать'});
          button.addEventListener('click', () =>
            confirmAction(
              {
                title: t('Использовать этот раздел для архива?'),
                description: t('Выбранный раздел станет хранилищем записей Dozor. Файлы на нём не форматируются.'),
                confirmLabel: 'Использовать',
                action: async () => {
                  await api('/disks/select', 'POST', {uuid: disk.uuid});
                  settings = await api('/settings');
                  renderDiskCurrent();
                  toast('Диск выбран', 'success');
                  refreshStatus().catch(() => {});
                  loadDisks().catch(() => {});
                },
                fallback: $('#load-disks'),
              },
              button
            )
          );
          row.append(el('div', {class: 'ui-item-actions'}, button));
        }
      }
      rows.append(el('li', null, row));
      for (const child of disk.children || []) addDisk(child, depth + 1);
    };
    for (const disk of result.disks) addDisk(disk, 0);
    if (!rows.children.length) diskBox.append(emptyState({description: t('Диски не найдены. Подключите диск и обновите список.'), compact: true}));
    else diskBox.append(rows);
    announce('Дисков в списке: ' + number.format(rows.children.length));
  }
  $('#load-disks').addEventListener('click', run(loadDisks));

  // ------------------------------------------------------------------ polling and start
  async function refreshStatus() {
    snapshot = await api('/status');
    lastOk = Date.now();
    failures = 0;
    renderStatus();
  }
  /** The status is polled every 10 s, only while the shell is visible and the tab is not hidden. A failure keeps the last snapshot. */
  async function poll() {
    if (polling || screen !== 'shell' || document.hidden) return;
    polling = true;
    try {
      await refreshStatus();
    } catch (error) {
      if (!(error && error.handled)) {
        failures += 1;
        renderConnection();
      }
    } finally {
      polling = false;
    }
  }
  window.setInterval(poll, 10000);
  document.addEventListener('visibilitychange', () => {
    if (document.hidden) livePlayer.stop();
    else {
      if (screen === 'shell') renderLive();
      poll();
    }
  });

  boot();
})();
