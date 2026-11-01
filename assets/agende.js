// Conveniences for the composer. Scheduling works without this file.
(function () {
  'use strict';
  // The draft stays in this browser until it is scheduled, so an expired session
  // doesn't lose it. Each account has its own, so nothing reappears under another
  // account, and it expires. It never leaves the browser.
  const draftPrefix = 'agende-draft:';
  const draftTTL = 7 * 24 * 60 * 60 * 1000;
  const account = document.body.dataset.account;
  const draftKey = draftPrefix + account;
  const params = new URLSearchParams(location.search);
  Object.keys(localStorage).filter(function (key) {
    return key.startsWith(draftPrefix);
  }).forEach(function (key) {
    try {
      if (Date.now() - JSON.parse(localStorage.getItem(key)).at > draftTTL) localStorage.removeItem(key);
    } catch (err) {
      localStorage.removeItem(key);
    }
  });
  if (location.pathname.startsWith('/result/') || params.get('done') === 'created' || params.get('done') === 'edited') {
    localStorage.removeItem(draftKey);
  }
  const logout = document.querySelector('.logout');
  if (logout) {
    logout.addEventListener('submit', function () { localStorage.removeItem(draftKey); });
  }
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
  const fixed = frequency.type === 'hidden';
  const dates = document.getElementById('dates');

  async function renderDates() {
    if (fixed) return;
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

  function saveDraft() {
    if (text.value === '' && cw.value === '') {
      localStorage.removeItem(draftKey);
      return;
    }
    localStorage.setItem(draftKey, JSON.stringify({ text: text.value, cw: cw.value, at: Date.now() }));
  }

  function restoreDraft() {
    if (text.value !== '' || params.has('from')) return;
    const saved = localStorage.getItem(draftKey);
    if (!saved) return;
    try {
      const draft = JSON.parse(saved);
      text.value = draft.text || '';
      cw.value = draft.cw || '';
    } catch (err) {
      localStorage.removeItem(draftKey);
    }
  }

  const delayedPreview = debounce(renderPreview, 400);
  text.addEventListener('input', function () { count(); delayedPreview(); saveDraft(); });
  cw.addEventListener('input', function () { count(); saveDraft(); });
  if (plain) plain.addEventListener('change', renderPreview);
  [when, timezone, frequency, countInput].forEach(function (el) {
    el.addEventListener('change', renderDates);
  });
  restoreDraft();
  count();
  renderPreview();
  renderDates();
}());
