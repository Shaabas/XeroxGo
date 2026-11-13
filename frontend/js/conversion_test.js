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

