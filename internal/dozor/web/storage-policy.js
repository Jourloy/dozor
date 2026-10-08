(() => {
  'use strict';
  const valid = value => Number.isInteger(value) && value >= 1 && value <= 99;

  window.DozorStoragePolicy = class {
    constructor(form, request) {
      this.form = form;
      this.request = request;
      this.input = form.querySelector('input');
      this.button = form.querySelector('[type="submit"]');
      this.feedback = form.querySelector('[role="status"]');
      this.errorBox = form.querySelector('[role="alert"]');
      this.errorText = form.querySelector('[data-storage-error]');
      this.retry = form.querySelector('[data-storage-retry]');
      this.generation = 0;
      this.input.addEventListener('input', () => {
        this.dirty = true;
        this.feedback.textContent = '';
        if (!this.loadError) this.errorBox.hidden = true;
        this.render();
      });
      this.retry.addEventListener('click', () => this.load());
      form.addEventListener('submit', event => { event.preventDefault(); this.save(); });
      this.reset();
    }
    reset() {
      this.generation++;
      this.value = null;
      this.dirty = false;
      this.busy = false;
      this.loading = false;
      this.loadError = false;
      this.input.value = '';
      this.feedback.textContent = 'Загружаем лимит диска…';
      this.errorBox.hidden = true;
      this.render();
    }
    render() {
      const value = Number(this.input.value);
      this.input.disabled = this.busy || this.value === null || this.loadError;
      this.input.setAttribute('aria-invalid', String(this.dirty && !valid(value)));
      this.button.disabled = this.input.disabled || !valid(value) || value === this.value;
      this.button.setAttribute('aria-busy', String(this.busy));
      this.retry.disabled = this.loading;
    }
    fail(message, loading) {
      this.errorText.textContent = message;
      this.retry.hidden = !loading;
      this.errorBox.hidden = false;
      if (loading) this.loadError = true;
      this.feedback.textContent = '';
    }
    async load() {
      if (this.busy || this.loading) return;
      const generation = this.generation;
      this.loading = true;
      this.render();
      try {
        const result = await this.request('/storage-policy');
        if (generation !== this.generation) return;
        if (!result || !valid(result.max_disk_usage_percent)) throw new Error('Invalid policy');
        const previous = this.value;
        this.value = result.max_disk_usage_percent;
        if (!this.dirty) this.input.value = String(this.value);
        this.loadError = false;
        this.errorBox.hidden = true;
        if (previous === null || previous !== this.value) this.feedback.textContent = '';
      } catch (error) {
        if (generation !== this.generation || error.handled) return;
        this.fail([404, 405, 501].includes(error.status)
          ? 'Обновите Dozor на Raspberry Pi, чтобы настроить лимит диска.'
          : 'Не удалось загрузить лимит диска. Повторите попытку.', true);
      } finally {
        if (generation === this.generation) { this.loading = false; this.render(); }
      }
    }
    async save() {
      const value = Number(this.input.value);
      if (this.busy || this.loadError || this.value === null || !valid(value) || value === this.value) return;
      // A GET already in flight must not overwrite the acknowledged PUT.
      const generation = ++this.generation;
      this.loading = false;
      this.busy = true;
      this.errorBox.hidden = true;
      this.feedback.textContent = '';
      this.render();
      try {
        const result = await this.request('/storage-policy', 'PUT', {max_disk_usage_percent: value});
        if (generation !== this.generation) return;
        if (!result || result.max_disk_usage_percent !== value) throw new Error('Unconfirmed policy');
        this.value = value;
        this.dirty = false;
        this.feedback.textContent = 'Лимит сохранён. Камеры продолжают запись.';
      } catch (error) {
        if (generation !== this.generation || error.handled) return;
        this.fail('Не удалось подтвердить сохранение лимита. Введённое значение оставлено в форме; повторите попытку.', false);
      } finally {
        if (generation === this.generation) { this.busy = false; this.render(); }
      }
    }
  };
})();
