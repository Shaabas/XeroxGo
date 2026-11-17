const test = require('node:test');
const assert = require('node:assert/strict');
const { confirmedOrderQuote, uploadOrderWithProgress } = require('./portal.js');

const tariff = { revision: 7 };
function item(price, revision = 7) {
  return { status: 'ready', authoritativePricePaise: price, tariffRevision: revision,
    quotePending: false, quoteError: '' };
}

test('checkout accepts previously confirmed multi-file quotes without re-quoting', () => {
  assert.deepEqual(confirmedOrderQuote([item(300), item(800), item(500)], tariff),
    { revision: 7, acceptedTotal: 1600 });
});

test('checkout requires ready, positive, current-revision cloud quotes', () => {
  assert.equal(confirmedOrderQuote([], tariff), null);
  assert.equal(confirmedOrderQuote([item(300)], null), null);
  assert.equal(confirmedOrderQuote([item(300)], { revision: 0 }), null);
  assert.equal(confirmedOrderQuote([item(300, 6)], tariff), null);
  assert.equal(confirmedOrderQuote([item(0)], tariff), null);
  assert.equal(confirmedOrderQuote([item(NaN)], tariff), null);
  assert.equal(confirmedOrderQuote([item(12.5)], tariff), null);
  assert.equal(confirmedOrderQuote([item(300), { ...item(500), quotePending: true }], tariff), null);
  assert.equal(confirmedOrderQuote([item(300), { ...item(500), quoteError: 'quote failed' }], tariff), null);
  assert.equal(confirmedOrderQuote([{ ...item(300), status: 'processing' }], tariff), null);
});

test('checkout rejects integer overflow rather than rounding the accepted price', () => {
  assert.equal(confirmedOrderQuote([item(Number.MAX_SAFE_INTEGER), item(1)], tariff), null);
});

class MockUploadRequest {
  static calls = [];
  constructor() {
    this.upload = new EventTarget();
    this.events = new EventTarget();
    this.status = 0;
    this.responseText = '';
    MockUploadRequest.calls.push(this);
  }
  open(method, url) { this.method = method; this.url = url; }
  addEventListener(name, fn) { this.events.addEventListener(name, fn); }
  send(form) { this.form = form; }
  finish(status, body) {
    this.status = status;
    this.responseText = body;
    this.events.dispatchEvent(new Event('load'));
  }
  fail(event) { this.events.dispatchEvent(new Event(event)); }
}

test('upload progress reflects measured bytes, then indeterminate server processing', async () => {
  const original = global.XMLHttpRequest;
  global.XMLHttpRequest = MockUploadRequest;
  try {
    const state = [];
    const form = { marker: true };
    const pending = uploadOrderWithProgress('/api/k/demo/upload', form,
      (stage, percent) => state.push([stage, percent]));
    const request = MockUploadRequest.calls.at(-1);
    assert.equal(request.method, 'POST');
    assert.equal(request.url, '/api/k/demo/upload');
    assert.equal(request.form, form);
    const event = new Event('progress');
    Object.defineProperties(event, {
      lengthComputable: { value: true },
      loaded: { value: 40 },
      total: { value: 100 }
    });
    request.upload.dispatchEvent(event);
    request.upload.dispatchEvent(new Event('load'));
    request.finish(200, JSON.stringify({ display_code: '1234' }));
    const response = await pending;
    assert.deepEqual(state, [['upload', 40], ['processing', null]]);
    assert.equal(response.ok, true);
    assert.equal((await response.json()).display_code, '1234');
  } finally { global.XMLHttpRequest = original; }
});

test('HTTP 409 remains a normal response for tariff-refresh logic', async () => {
  const original = global.XMLHttpRequest;
  global.XMLHttpRequest = MockUploadRequest;
  try {
    const pending = uploadOrderWithProgress('/api/k/demo/upload', {}, () => {});
    MockUploadRequest.calls.at(-1).finish(409, 'Price changed');
    const response = await pending;
    assert.equal(response.ok, false);
    assert.equal(response.status, 409);
    assert.equal(await response.text(), 'Price changed');
  } finally { global.XMLHttpRequest = original; }
});

test('ambiguous network failure never silently resubmits an order', async () => {
  const original = global.XMLHttpRequest;
  global.XMLHttpRequest = MockUploadRequest;
  try {
    const count = MockUploadRequest.calls.length;
    const pending = uploadOrderWithProgress('/api/k/demo/upload', {}, () => {});
    MockUploadRequest.calls.at(-1).fail('error');
    await assert.rejects(pending, /might have been created/);
    assert.equal(MockUploadRequest.calls.length, count + 1);
  } finally { global.XMLHttpRequest = original; }
});
