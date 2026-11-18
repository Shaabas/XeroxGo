const test = require('node:test');
const assert = require('node:assert/strict');
const { cloudUploadPDF, uploadSignedFile } = require('./cloud_upload.js');

class FakeRequest {
  constructor() {
    this.upload = new EventTarget();
    this.listeners = new Map();
    this.headers = {};
    FakeRequest.instances.push(this);
  }
  open(method, url) { this.method = method; this.url = url; }
  setRequestHeader(key, value) { this.headers[key] = value; }
  addEventListener(event, fn) { this.listeners.set(event, fn); }
  send(payload) { this.payload = payload; }
  finish(code) { this.status = code; this.listeners.get('load')(); }
  fail() { this.listeners.get('error')(); }
}
FakeRequest.instances = [];

test('cloud upload puts bytes directly to signed Storage URL with real progress', async () => {
  const file = { name: 'x.pdf', type: 'application/pdf', size: 100 };
  const progress = [];
  const pending = uploadSignedFile('https://storage.example/sign?token=secret',
    file, n => progress.push(n), () => new FakeRequest());
  const req = FakeRequest.instances.at(-1);
  assert.equal(req.method, 'PUT');
  assert.equal(req.url, 'https://storage.example/sign?token=secret');
  assert.equal(req.payload, file);
  assert.equal(req.headers['Content-Type'], 'application/pdf');
  const event = new Event('progress');
  Object.defineProperties(event, {
    lengthComputable: { value: true }, loaded: { value: 33 }, total: { value: 100 }
  });
  req.upload.dispatchEvent(event);
  req.finish(200);
  await pending;
  assert.deepEqual(progress, [33]);
});

test('direct upload sends PDF bytes once and references receipt for finalize', async () => {
  const file = { name: 'x.pdf', type: 'application/pdf', size: 100 };
  const calls = [];
  const fakeFetch = async (url, options) => {
    calls.push({ url, options });
    if (url.endsWith('/init')) return { ok: true, json: async () => ({
      upload_url: 'https://storage.example/sign?token=test',
      upload_id: 'id123', upload_token: 'receipt-token'
    }) };
    return { ok: true, json: async () => ({
      upload_id: 'id123', status: 'UPLOADED', sha256: 'testhash'
    }) };
  };
  const pending = cloudUploadPDF(file, 'kiosk-test', () => {}, fakeFetch,
    () => new FakeRequest());
  // async function waits for the first mocked API request before constructing XHR.
  await new Promise(resolve => setImmediate(resolve));
  const req = FakeRequest.instances.at(-1);
  assert.equal(req.payload, file);
  req.finish(200);
  const result = await pending;
  assert.deepEqual(result, { uploadId: 'id123', checksum: 'testhash' });
  assert.deepEqual(calls.map(c => c.url), [
    '/api/documents/uploads/init', '/api/documents/uploads/complete'
  ]);
  assert.deepEqual(JSON.parse(calls[1].options.body), {
    upload_id: 'id123', upload_token: 'receipt-token'
  });
});

test('rejects invalid files before requesting upload authorization', async () => {
  let calls = 0;
  await assert.rejects(cloudUploadPDF({ name: 'x.docx', type: 'application/pdf', size: 0 },
    'demo', () => {}, async () => { calls++; }), /Select a PDF/);
  assert.equal(calls, 0);
});
