// Conveniences for the composer. Scheduling works without this file.
(function () {
  'use strict';
  const form = document.getElementById('compose');
  if (!form) return;
  const csrf = form.dataset.csrf;
  const max = Number(form.dataset.max);
  const text = document.getElementById('text');
  const cw = document.getElementById('cw');
  const left = document.getElementById('left');
  const plain = document.getElementById('plain');
  const previewBox = document.getElementById('preview-box');
  const preview = document.getElementById('preview');
  const urls = /https?:\/\/\S+/g;

  function debounce(fn, ms) {
    let timer;
    return function () {
      clearTimeout(timer);
      timer = setTimeout(fn, ms);
    };
  }

  // Mastodon counts every URL as 23 characters.
  function count() {
    const body = text.value.replace(urls, 'x'.repeat(23));
    left.textContent = max - Array.from(body).length - Array.from(cw.value).length;
  }

  async function renderPreview() {
    if (!preview) return;
    const on = text.value.trim() !== '' && !plain.checked;
    previewBox.hidden = !on;
    if (!on) return;
    const resp = await fetch('/preview', {
      method: 'POST',
      headers: { 'X-CSRF-Token': csrf, 'Content-Type': 'text/plain' },
      body: text.value,
    });
    if (resp.ok) preview.innerHTML = await resp.text(); // sanitized by the server
  }

  const when = document.getElementById('when');
  const timezone = document.getElementById('timezone');
  const frequency = document.getElementById('frequency');
  const countInput = document.getElementById('count');
  const datesBox = document.getElementById('dates-box');
  const dates = document.getElementById('dates');

  async function renderDates() {
    const repeated = frequency.value !== 'once';
    countInput.closest('label').hidden = !repeated;
    datesBox.hidden = !repeated;
    if (!repeated || !when.value) return;
    const resp = await fetch('/dates', {
      method: 'POST',
      headers: { 'X-CSRF-Token': csrf, 'Content-Type': 'application/json' },
      body: JSON.stringify({
        when: when.value,
        timezone: timezone.value,
        frequency: frequency.value,
        count: Number(countInput.value),
      }),
    });
    if (!resp.ok) return;
    const out = await resp.json();
    dates.replaceChildren(...out.dates.map(function (d) {
      const li = document.createElement('li');
      li.textContent = d;
      return li;
    }));
  }

  const delayedPreview = debounce(renderPreview, 400);
  text.addEventListener('input', function () { count(); delayedPreview(); });
  cw.addEventListener('input', count);
  if (plain) plain.addEventListener('change', renderPreview);
  [when, timezone, frequency, countInput].forEach(function (el) {
    el.addEventListener('change', renderDates);
  });
  count();
  renderPreview();
  renderDates();
}());
