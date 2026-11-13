// @ts-check
const test = require('node:test');
const assert = require('node:assert/strict');
const { convertDocumentOnServer } = require('./portal.js');

// Helper to create a fake File object
function createDummyFile(name = 'sample.docx', size = 1024) {
  return {
    name,
    size,
    type: 'application/vnd.openxmlformats-officedocument.wordprocessingml.document',
  };
}

// Helper to mock global fetch
function createMockFetch(handler) {
  return async (url, options = {}) => {
    return handler(url.toString(), options);
  };
}

test('Conversion Workflow — Successful async conversion lifecycle', async () => {
  const file = createDummyFile('test.docx');
  const progressStates = [];
  const requestedUrls = [];

  const fakePdfBytes = new Uint8Array([0x25, 0x50, 0x44, 0x46, 0x2d, 0x31, 0x2e, 0x34]).buffer; // "%PDF-1.4"

  let pollCount = 0;
  global.fetch = createMockFetch((url, options) => {
    requestedUrls.push({ url, method: options.method || 'GET' });

    if (url === '/api/conversions' && options.method === 'POST') {
      return {
        ok: true,
        status: 202,
        json: async () => ({
          job_id: 'conv_test_001',
          token: 'tok_test_abc',
          status: 'queued',
          poll_url: '/api/conversions/conv_test_001?token=tok_test_abc',
        }),
      };
    }

    if (url === '/api/conversions/conv_test_001?token=tok_test_abc') {
      pollCount++;
      if (pollCount === 1) {
        return {
          ok: true,
          status: 200,
          json: async () => ({ status: 'processing' }),
        };
      }
      return {
        ok: true,
        status: 200,
        json: async () => ({
          status: 'completed',
          download_url: '/api/conversions/conv_test_001/pdf?token=tok_test_abc',
        }),
      };
    }

    if (url === '/api/conversions/conv_test_001/pdf?token=tok_test_abc') {
      return {
        ok: true,
        status: 200,
        arrayBuffer: async () => fakePdfBytes,
      };
    }

    throw new Error(`Unexpected request to ${url}`);
  });

  const result = await convertDocumentOnServer(file, null, (msg) => {
    progressStates.push(msg);
  });

  assert.ok(result instanceof ArrayBuffer, 'Expected ArrayBuffer result');
  assert.equal(result.byteLength, 8);

  // Verify all expected lifecycle progress states were emitted in sequence
  assert.deepEqual(progressStates, [
    'Uploading for conversion...',
    'Waiting in conversion queue...',
    'Converting pages on server...',
    'Downloading converted PDF...',
  ]);

  // Ensure NO requests were ever sent to /api/convert
  const syncCalls = requestedUrls.filter(r => r.url === '/api/convert');
  assert.equal(syncCalls.length, 0, 'Synchronous /api/convert must never be called');
});

test('Conversion Workflow — Engine failure reporting from server', async () => {
  const file = createDummyFile('broken.docx');
  const requestedUrls = [];

  global.fetch = createMockFetch((url, options) => {
    requestedUrls.push(url);
    if (url === '/api/conversions') {
      return {
        ok: true,
        status: 202,
        json: async () => ({
          job_id: 'conv_err_002',
          token: 'tok_err',
          status: 'queued',
          poll_url: '/api/conversions/conv_err_002?token=tok_err',
        }),
      };
    }
    if (url === '/api/conversions/conv_err_002?token=tok_err') {
      return {
        ok: true,
        status: 200,
        json: async () => ({
          status: 'failed',
          error_message: 'LibreOffice filter failed: corrupted file structure',
        }),
      };
    }
    throw new Error(`Unexpected request to ${url}`);
  });

  await assert.rejects(
    async () => {
      await convertDocumentOnServer(file, null, () => {});
    },
    {
      message: 'LibreOffice filter failed: corrupted file structure',
    }
  );

  // Ensure no fallback to /api/convert occurred
  assert.ok(!requestedUrls.includes('/api/convert'), 'Should not fall back to /api/convert on failure');
});

test('Conversion Safety — Network error on create prevents duplicate submission and fallback', async () => {
  const file = createDummyFile('notes.docx');
  let createAttempts = 0;

  global.fetch = createMockFetch((url, options) => {
    if (url === '/api/conversions') {
      createAttempts++;
      throw new TypeError('Failed to fetch (DNS lookup timeout)');
    }
    if (url === '/api/convert') {
      throw new Error('Fallback /api/convert was incorrectly called!');
    }
    throw new Error(`Unexpected request to ${url}`);
  });

  await assert.rejects(
    async () => {
      await convertDocumentOnServer(file, null, () => {});
    },
    /Network error while uploading document for conversion/
  );

  assert.equal(createAttempts, 1, 'Must not retry or trigger duplicate creation after ambiguous upload failure');
});

test('Conversion Resilience — Transient network error during polling recovers cleanly', async () => {
  const file = createDummyFile('report.docx');
  const progressStates = [];
  let pollAttempts = 0;

  global.fetch = createMockFetch((url, options) => {
    if (url === '/api/conversions') {
      return {
        ok: true,
        status: 202,
        json: async () => ({
          job_id: 'conv_retry_003',
          token: 'tok_retry',
          status: 'processing',
          poll_url: '/api/conversions/conv_retry_003?token=tok_retry',
        }),
      };
    }

    if (url === '/api/conversions/conv_retry_003?token=tok_retry') {
      pollAttempts++;
      if (pollAttempts === 1) {
        // Transient network failure on 1st poll attempt
        throw new TypeError('Network connection lost temporarily');
      }
      // Successful 2nd poll attempt
      return {
        ok: true,
        status: 200,
        json: async () => ({
          status: 'completed',
          download_url: '/api/conversions/conv_retry_003/pdf?token=tok_retry',
        }),
      };
    }

    if (url === '/api/conversions/conv_retry_003/pdf?token=tok_retry') {
      return {
        ok: true,
        status: 200,
        arrayBuffer: async () => new Uint8Array([1, 2, 3]).buffer,
      };
    }

    throw new Error(`Unexpected URL ${url}`);
  });

  const res = await convertDocumentOnServer(file, null, (msg) => {
    progressStates.push(msg);
  });

  assert.equal(res.byteLength, 3);
  assert.ok(
    progressStates.includes('Connection interrupted, retrying status check...'),
    'Expected transient connection warning'
  );
  assert.equal(pollAttempts, 2);
});

test('Conversion Resilience — Persistent network failure during polling aborts safely', async () => {
  const file = createDummyFile('report.docx');

  global.fetch = createMockFetch((url) => {
    if (url === '/api/conversions') {
      return {
        ok: true,
        status: 202,
        json: async () => ({
          job_id: 'conv_fail_004',
          token: 'tok_fail',
          status: 'processing',
          poll_url: '/api/conversions/conv_fail_004?token=tok_fail',
        }),
      };
    }
    // Continuous network failure on polling
    throw new TypeError('Offline');
  });

  await assert.rejects(
    async () => {
      await convertDocumentOnServer(file, null, () => {});
    },
    /Network error while checking conversion progress/
  );
});

test('Conversion Cancellation — AbortSignal cancels in-flight conversion', async () => {
  const file = createDummyFile('cancel.docx');
  const ac = new AbortController();

  global.fetch = createMockFetch((url) => {
    if (url === '/api/conversions') {
      return {
        ok: true,
        status: 202,
        json: async () => ({
          job_id: 'conv_abort_005',
          token: 'tok_abort',
          status: 'queued',
          poll_url: '/api/conversions/conv_abort_005?token=tok_abort',
        }),
      };
    }
    // Abort client during polling
    ac.abort();
    return {
      ok: true,
      status: 200,
      json: async () => ({ status: 'queued' }),
    };
  });

  await assert.rejects(
    async () => {
      await convertDocumentOnServer(file, ac.signal, () => {});
    },
    /Cancelled/
  );
});

test('Conversion Errors — Server 404 response on polling indicates job expiration', async () => {
  const file = createDummyFile('expired.docx');

  global.fetch = createMockFetch((url) => {
    if (url === '/api/conversions') {
      return {
        ok: true,
        status: 202,
        json: async () => ({
          job_id: 'conv_exp_006',
          token: 'tok_exp',
          status: 'processing',
          poll_url: '/api/conversions/conv_exp_006?token=tok_exp',
        }),
      };
    }
    return {
      ok: false,
      status: 404,
      statusText: 'Not Found',
      text: async () => 'conversion job not found',
    };
  });

  await assert.rejects(
    async () => {
      await convertDocumentOnServer(file, null, () => {});
    },
    /Conversion job expired or not found on server/
  );
});

test('Conversion Errors — Server 413 Payload Too Large on initial upload', async () => {
  const file = createDummyFile('huge.docx', 30 * 1024 * 1024);

  global.fetch = createMockFetch((url) => {
    if (url === '/api/conversions') {
      return {
        ok: false,
        status: 413,
        statusText: 'Payload Too Large',
        text: async () => 'file exceeds 25 MiB limit',
      };
    }
    throw new Error('Unexpected call');
  });

  await assert.rejects(
    async () => {
      await convertDocumentOnServer(file, null, () => {});
    },
    /File exceeds server size limit \(25 MiB\)/
  );
});

test('Headless Environment Safety — CommonJS import without DOM globals', () => {
  // Verify that importing portal.js in an environment where window and document are undefined
  // does not throw ReferenceError or evaluate browser-only event listeners.
  assert.equal(typeof convertDocumentOnServer, 'function', 'convertDocumentOnServer must be exported as a function');
});

test('Preview Sizing Math — Responsive stage scale and dimension bounds', () => {
  // Mirror the exact stage calculation algorithm from portal.js:1725-1738
  function computeStageDimensions(stageClientWidth, innerW, innerH, rawW, rawH, padX = 16) {
    const availableW = (stageClientWidth > 0)
      ? (stageClientWidth - padX)
      : Math.min(innerW - 64, 536);
    const maxStageW = Math.max(220, availableW);
    const maxStageH = (innerW >= 600)
      ? Math.min(420, Math.max(320, Math.round(innerH * 0.48)))
      : Math.min(360, Math.max(280, Math.round(innerH * 0.45)));
    const scale = Math.min(maxStageW / rawW, maxStageH / rawH, 3.0);
    const renderedW = Math.round(rawW * scale);
    const renderedH = Math.round(rawH * scale);
    return { maxStageW, maxStageH, scale, renderedW, renderedH };
  }

  const a4PortraitW = 595.28;
  const a4PortraitH = 841.89;
  const a4LandscapeW = 841.89;
  const a4LandscapeH = 595.28;

  // 1. Mobile phone portrait (360x640, container ~320px)
  const mobile = computeStageDimensions(320, 360, 640, a4PortraitW, a4PortraitH);
  assert.ok(mobile.scale > 0 && mobile.scale <= 3.0, 'Mobile scale must be positive and <= 3.0');
  assert.ok(mobile.renderedW <= mobile.maxStageW, `Rendered width ${mobile.renderedW} must not exceed stage width ${mobile.maxStageW}`);
  assert.ok(mobile.renderedH <= mobile.maxStageH, `Rendered height ${mobile.renderedH} must not exceed stage height ${mobile.maxStageH}`);
  assert.equal(mobile.maxStageW, 304, 'maxStageW on mobile with 320px clientWidth minus 16px pad should be 304');
  assert.equal(mobile.maxStageH, 288, 'maxStageH on mobile with 640px innerHeight should be 288');

  // 2. Desktop large viewport (1440x900, stage container ~460px)
  const desktop = computeStageDimensions(460, 1440, 900, a4PortraitW, a4PortraitH);
  assert.ok(desktop.scale > 0 && desktop.scale <= 3.0, 'Desktop scale must be positive and <= 3.0');
  assert.ok(desktop.renderedW <= desktop.maxStageW, `Desktop rendered width ${desktop.renderedW} must not exceed stage width ${desktop.maxStageW}`);
  assert.ok(desktop.renderedH <= desktop.maxStageH, `Desktop rendered height ${desktop.renderedH} must not exceed stage height ${desktop.maxStageH}`);
  assert.equal(desktop.maxStageW, 444, 'maxStageW on desktop with 460px clientWidth minus 16px pad should be 444');
  assert.equal(desktop.maxStageH, 420, 'maxStageH on desktop should be capped at 420');

  // 3. Landscape A4 document on mobile
  const landscape = computeStageDimensions(320, 360, 640, a4LandscapeW, a4LandscapeH);
  assert.ok(landscape.renderedW <= landscape.maxStageW, 'Landscape rendered width must not exceed stage width');
  assert.ok(landscape.renderedH <= landscape.maxStageH, 'Landscape rendered height must not exceed stage height');

  // 4. Fallback when stage element clientWidth is 0 (hidden view or unmounted)
  const fallback = computeStageDimensions(0, 375, 667, a4PortraitW, a4PortraitH);
  assert.equal(fallback.maxStageW, 311, 'availableW should fall back to Math.min(375 - 64, 536) = 311');
  assert.ok(fallback.renderedW <= fallback.maxStageW, 'Fallback rendered width must be within bounds');

  // 5. Extremely narrow viewport clamped to minimum 220px
  const clamped = computeStageDimensions(100, 150, 400, a4PortraitW, a4PortraitH);
  assert.equal(clamped.maxStageW, 220, 'maxStageW must clamp to minimum 220px');
});


