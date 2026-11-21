// @ts-check
const test = require('node:test');
const assert = require('node:assert/strict');
const { calculateEstimate, parsePositiveInteger, parseRupeesToPaise, initPricingController } = require('./admin.js');

// Standard default rates in paise: B&W 300/500, Color 800/1000
const defaultRates = {
  bwSimplex: 300,
  bwDuplex: 500,
  colorSimplex: 800,
  colorDuplex: 1000,
};

test('Estimator Validation — parsePositiveInteger', () => {
  assert.equal(parsePositiveInteger('10'), 10);
  assert.equal(parsePositiveInteger('1'), 1);
  assert.equal(parsePositiveInteger('  5  '), 5);
  assert.equal(parsePositiveInteger(3), 3);

  // Safe integer boundaries
  assert.equal(parsePositiveInteger(Number.MAX_SAFE_INTEGER), Number.MAX_SAFE_INTEGER);
  assert.equal(parsePositiveInteger(String(Number.MAX_SAFE_INTEGER)), Number.MAX_SAFE_INTEGER);
  assert.equal(parsePositiveInteger(Number.MAX_SAFE_INTEGER + 1), null);
  assert.equal(parsePositiveInteger('9007199254740992'), null);
  assert.equal(parsePositiveInteger('9999999999999999999999999999'), null);

  // Invalid cases must return null
  assert.equal(parsePositiveInteger('0'), null);
  assert.equal(parsePositiveInteger('-1'), null);
  assert.equal(parsePositiveInteger('1.5'), null);
  assert.equal(parsePositiveInteger('abc'), null);
  assert.equal(parsePositiveInteger(''), null);
  assert.equal(parsePositiveInteger(null), null);
  assert.equal(parsePositiveInteger(undefined), null);
});

test('Estimator Validation — parseRupeesToPaise', () => {
  assert.equal(parseRupeesToPaise('3'), 300);
  assert.equal(parseRupeesToPaise('3.00'), 300);
  assert.equal(parseRupeesToPaise('0.50'), 50);
  assert.equal(parseRupeesToPaise('0.05'), 5);
  assert.equal(parseRupeesToPaise('10.25'), 1025);
  assert.equal(parseRupeesToPaise('1.14'), 114);

  // Safe integer boundary and overflow
  assert.equal(parseRupeesToPaise('99999999999999999.99'), null);
  assert.equal(parseRupeesToPaise('9007199254740992.00'), null);

  // Invalid or nonpositive cases must return null
  assert.equal(parseRupeesToPaise('0'), null);
  assert.equal(parseRupeesToPaise('0.00'), null);
  assert.equal(parseRupeesToPaise('-5'), null);
  assert.equal(parseRupeesToPaise('1.234'), null);
  assert.equal(parseRupeesToPaise('abc'), null);
  assert.equal(parseRupeesToPaise(''), null);
  assert.equal(parseRupeesToPaise(null), null);
});

test('Estimator Acceptance Table — All 7 Specified Scenarios', () => {
  // Scenario 1: 10 pages, 1-up, Single, 1 copy, B&W -> 10 sides, 10 sheets, ₹30.00
  const s1 = calculateEstimate({
    pages: 10,
    nUp: 1,
    sides: 'single',
    copies: 1,
    mode: 'bw',
    rates: defaultRates,
  });
  assert.equal(s1.valid, true);
  assert.equal(s1.printedSidesDisplay, '10');
  assert.equal(s1.sheetsDisplay, '10');
  assert.equal(s1.totalCostDisplay, '₹30.00');

  // Scenario 2: 2 pages, 1-up, Double, 1 copy, B&W -> 2 sides, 1 sheet, ₹5.00
  const s2 = calculateEstimate({
    pages: 2,
    nUp: 1,
    sides: 'double',
    copies: 1,
    mode: 'bw',
    rates: defaultRates,
  });
  assert.equal(s2.valid, true);
  assert.equal(s2.printedSidesDisplay, '2');
  assert.equal(s2.sheetsDisplay, '1');
  assert.equal(s2.totalCostDisplay, '₹5.00');

  // Scenario 3: 3 pages, 1-up, Double, 1 copy, B&W -> 3 sides, 2 sheets, ₹8.00
  const s3 = calculateEstimate({
    pages: 3,
    nUp: 1,
    sides: 'double',
    copies: 1,
    mode: 'bw',
    rates: defaultRates,
  });
  assert.equal(s3.valid, true);
  assert.equal(s3.printedSidesDisplay, '3');
  assert.equal(s3.sheetsDisplay, '2');
  assert.equal(s3.totalCostDisplay, '₹8.00');

  // Scenario 4: 3 pages, 1-up, Double, 2 copies, B&W -> 3 sides, 4 sheets, ₹16.00
  const s4 = calculateEstimate({
    pages: 3,
    nUp: 1,
    sides: 'double',
    copies: 2,
    mode: 'bw',
    rates: defaultRates,
  });
  assert.equal(s4.valid, true);
  assert.equal(s4.printedSidesDisplay, '3');
  assert.equal(s4.sheetsDisplay, '4');
  assert.equal(s4.totalCostDisplay, '₹16.00');

  // Scenario 5: 10 pages, 2-up, Double, 2 copies, B&W -> 5 sides, 6 sheets, ₹26.00
  const s5 = calculateEstimate({
    pages: 10,
    nUp: 2,
    sides: 'double',
    copies: 2,
    mode: 'bw',
    rates: defaultRates,
  });
  assert.equal(s5.valid, true);
  assert.equal(s5.printedSidesDisplay, '5');
  assert.equal(s5.sheetsDisplay, '6');
  assert.equal(s5.totalCostDisplay, '₹26.00');

  // Scenario 6: 3 pages, 1-up, Double, 1 copy, Color -> 3 sides, 2 sheets, ₹18.00
  const s6 = calculateEstimate({
    pages: 3,
    nUp: 1,
    sides: 'double',
    copies: 1,
    mode: 'color',
    rates: defaultRates,
  });
  assert.equal(s6.valid, true);
  assert.equal(s6.printedSidesDisplay, '3');
  assert.equal(s6.sheetsDisplay, '2');
  assert.equal(s6.totalCostDisplay, '₹18.00');

  // Scenario 7: 10 pages, 1-up, Single, 1 copy, Color -> 10 sides, 10 sheets, ₹80.00
  const s7 = calculateEstimate({
    pages: 10,
    nUp: 1,
    sides: 'single',
    copies: 1,
    mode: 'color',
    rates: defaultRates,
  });
  assert.equal(s7.valid, true);
  assert.equal(s7.printedSidesDisplay, '10');
  assert.equal(s7.sheetsDisplay, '10');
  assert.equal(s7.totalCostDisplay, '₹80.00');
});

test('Estimator Validation — Malformed and nonnumeric quantities fail closed', () => {
  // Invalid pages
  const invalidPages = [0, -1, 1.5, 'abc', '', null, undefined];
  for (const p of invalidPages) {
    const res = calculateEstimate({
      pages: p,
      copies: 1,
      nUp: 1,
      sides: 'single',
      mode: 'bw',
      rates: defaultRates,
    });
    assert.equal(res.valid, false);
    assert.equal(res.printedSidesDisplay, '--');
    assert.equal(res.sheetsDisplay, '--');
    assert.equal(res.totalCostDisplay, '--');
  }

  // Invalid copies
  const invalidCopies = [0, -2, 2.5, 'xyz', '', null, undefined];
  for (const c of invalidCopies) {
    const res = calculateEstimate({
      pages: 10,
      copies: c,
      nUp: 1,
      sides: 'single',
      mode: 'bw',
      rates: defaultRates,
    });
    assert.equal(res.valid, false);
    assert.equal(res.printedSidesDisplay, '--');
    assert.equal(res.sheetsDisplay, '--');
    assert.equal(res.totalCostDisplay, '--');
  }
});

test('Estimator Validation — Unavailable or invalid tariffs display -- for total', () => {
  // Simplex tariff missing for single-sided
  const res1 = calculateEstimate({
    pages: 10,
    copies: 1,
    nUp: 1,
    sides: 'single',
    mode: 'bw',
    rates: { bwSimplex: null, bwDuplex: 500 },
  });
  assert.equal(res1.valid, true);
  assert.equal(res1.printedSidesDisplay, '10');
  assert.equal(res1.sheetsDisplay, '10');
  assert.equal(res1.totalCostDisplay, '--');

  // Duplex tariff missing for double-sided
  const res2 = calculateEstimate({
    pages: 4,
    copies: 1,
    nUp: 1,
    sides: 'double',
    mode: 'bw',
    rates: { bwSimplex: 300, bwDuplex: null },
  });
  assert.equal(res2.valid, true);
  assert.equal(res2.printedSidesDisplay, '4');
  assert.equal(res2.sheetsDisplay, '2');
  assert.equal(res2.totalCostDisplay, '--');

  // Color rates missing when mode is Color
  const res3 = calculateEstimate({
    pages: 10,
    copies: 1,
    nUp: 1,
    sides: 'single',
    mode: 'color',
    rates: { bwSimplex: 300, bwDuplex: 500, colorSimplex: null, colorDuplex: null },
  });
  assert.equal(res3.valid, true);
  assert.equal(res3.printedSidesDisplay, '10');
  assert.equal(res3.sheetsDisplay, '10');
  assert.equal(res3.totalCostDisplay, '--');
});

test('Estimator Validation — Safe integer overflow fails closed without inaccurate values', () => {
  // 1. Total sheets exceeds safe integer range -> fails closed with placeholder for all fields
  const unsafeSheets = calculateEstimate({
    pages: Number.MAX_SAFE_INTEGER,
    copies: 2,
    nUp: 1,
    sides: 'single',
    mode: 'bw',
    rates: defaultRates
  });
  assert.equal(unsafeSheets.valid, false);
  assert.equal(unsafeSheets.printedSidesDisplay, '--');
  assert.equal(unsafeSheets.sheetsDisplay, '--');
  assert.equal(unsafeSheets.totalCostDisplay, '--');

  // 2. Physical sheets are safe, but total cost in paise exceeds safe integer range
  // 10^7 pages * 10^7 copies = 10^14 sheets (safe integer < 9e15)
  // Total cost in paise = 10^14 * 300 = 3 * 10^16 (exceeds MAX_SAFE_INTEGER)
  const unsafeCost = calculateEstimate({
    pages: 10000000,
    copies: 10000000,
    nUp: 1,
    sides: 'single',
    mode: 'bw',
    rates: defaultRates
  });
  assert.equal(unsafeCost.valid, true);
  assert.equal(unsafeCost.printedSidesDisplay, '10000000');
  assert.equal(unsafeCost.sheetsDisplay, '100000000000000');
  assert.equal(unsafeCost.totalCostDisplay, '--', 'Exceeded safe-integer cost must display placeholder --');
});

test('Estimator Rates Preview — Unsaved rates preview correctly', () => {
  // Admin previews an unsaved B&W simplex rate of ₹4.00 (400 paise)
  const previewRates = {
    bwSimplex: 400,
    bwDuplex: 500,
    colorSimplex: 800,
    colorDuplex: 1000,
  };
  const res = calculateEstimate({
    pages: 10,
    copies: 1,
    nUp: 1,
    sides: 'single',
    mode: 'bw',
    rates: previewRates,
  });
  assert.equal(res.printedSidesDisplay, '10');
  assert.equal(res.sheetsDisplay, '10');
  assert.equal(res.totalCostDisplay, '₹40.00');
});

test('Estimator Live Updates, Event Handlers & API Isolation — Real Controller Integration', async () => {
  // 1. Set up lightweight DOM mock
  const listeners = {};
  function createMockElement(id, initialProps = {}) {
    listeners[id] = {};
    return {
      id,
      value: initialProps.value !== undefined ? initialProps.value : '',
      textContent: initialProps.textContent !== undefined ? initialProps.textContent : '',
      placeholder: initialProps.placeholder !== undefined ? initialProps.placeholder : '',
      disabled: initialProps.disabled !== undefined ? initialProps.disabled : false,
      checked: initialProps.checked !== undefined ? initialProps.checked : false,
      addEventListener(evt, fn) {
        if (!listeners[id][evt]) listeners[id][evt] = [];
        listeners[id][evt].push(fn);
      },
      trigger(evt, eventObj = {}) {
        const fns = listeners[id][evt] || [];
        for (const fn of fns) {
          fn({
            preventDefault: () => {},
            target: this,
            ...eventObj
          });
        }
      }
    };
  }

  const elements = {
    'pricing-form': createMockElement('pricing-form'),
    'price-bw-simplex-input': createMockElement('price-bw-simplex-input', { value: '' }),
    'price-bw-duplex-input': createMockElement('price-bw-duplex-input', { value: '' }),
    'price-color-simplex-input': createMockElement('price-color-simplex-input', { value: '', disabled: true }),
    'price-color-duplex-input': createMockElement('price-color-duplex-input', { value: '', disabled: true }),
    'pricing-color-enabled-toggle': createMockElement('pricing-color-enabled-toggle', { checked: false }),
    'pricing-save-btn': createMockElement('pricing-save-btn', { textContent: 'Save Rates' }),
    'calc-pages-input': createMockElement('calc-pages-input', { value: '10' }),
    'calc-copies-input': createMockElement('calc-copies-input', { value: '1' }),
    'calc-nup-select': createMockElement('calc-nup-select', { value: '1' }),
    'calc-sides-select': createMockElement('calc-sides-select', { value: 'single' }),
    'calc-mode-select': createMockElement('calc-mode-select', { value: 'bw' }),
    'calc-result-sides': createMockElement('calc-result-sides', { textContent: '10' }),
    'calc-result-sheets': createMockElement('calc-result-sheets', { textContent: '10' }),
    'calc-result-total': createMockElement('calc-result-total', { textContent: '--' })
  };

  const mockDocument = {
    getElementById: (id) => elements[id] || null
  };

  // 2. Mock network calls at the request boundary
  const networkCalls = [];
  let serverPricingResponse = {
    bw_simplex_price_paise: 300,
    bw_duplex_price_paise: 500,
    color_simplex_price_paise: 800,
    color_duplex_price_paise: 1000,
    color_enabled: false
  };
  let shouldFailPricing = false;

  async function mockApiRequest(endpoint, options = {}) {
    const method = options.method || 'GET';
    const body = options.body ? (typeof options.body === 'string' ? JSON.parse(options.body) : options.body) : null;
    networkCalls.push({ endpoint, method, body });

    if (endpoint === '/api/admin/pricing') {
      if (method === 'GET') {
        if (shouldFailPricing) {
          return { ok: false, text: async () => JSON.stringify({ error: 'Database unavailable' }) };
        }
        return { ok: true, text: async () => JSON.stringify(serverPricingResponse) };
      }
      if (method === 'PUT') {
        return { ok: true, text: async () => JSON.stringify({ ok: true }) };
      }
    }
    return { ok: false, text: async () => JSON.stringify({ error: 'Not found' }) };
  }

  // 3. Initialize real production pricing controller
  const controller = initPricingController({
    document: mockDocument,
    apiRequest: mockApiRequest,
    showToast: () => {},
    alert: () => {}
  });
  assert.ok(controller, 'Pricing controller must be initialized');

  // Verify before pricing load: initial total is placeholder '--'
  assert.equal(elements['calc-result-total'].textContent, '--', 'Calculated total must initially be -- before server pricing loads');

  // Load canonical server pricing
  await controller.loadPricing();

  // Verify loaded canonical rates in inputs
  assert.equal(elements['price-bw-simplex-input'].value, '3.00');
  assert.equal(elements['price-bw-duplex-input'].value, '5.00');
  assert.equal(elements['price-color-simplex-input'].value, '8.00');
  assert.equal(elements['price-color-duplex-input'].value, '10.00');
  assert.equal(elements['pricing-color-enabled-toggle'].checked, false);

  // Behavior: Color inputs stay disabled while kiosk color is disabled
  assert.equal(elements['price-color-simplex-input'].disabled, true, 'Color simplex input must be disabled when color is off');
  assert.equal(elements['price-color-duplex-input'].disabled, true, 'Color duplex input must be disabled when color is off');

  // Loaded default calculation: 10 pages, 1 copy, 1-up, single-sided, B&W -> 10 sides, 10 sheets, ₹30.00
  assert.equal(elements['calc-result-sides'].textContent, '10');
  assert.equal(elements['calc-result-sheets'].textContent, '10');
  assert.equal(elements['calc-result-total'].textContent, '₹30.00');

  // Behavior: Changing document pages recalculates displayed total
  elements['calc-pages-input'].value = '20';
  elements['calc-pages-input'].trigger('input');
  assert.equal(elements['calc-result-sides'].textContent, '20');
  assert.equal(elements['calc-result-sheets'].textContent, '20');
  assert.equal(elements['calc-result-total'].textContent, '₹60.00');

  // Behavior: Changing copies recalculates
  elements['calc-copies-input'].value = '2';
  elements['calc-copies-input'].trigger('input');
  assert.equal(elements['calc-result-sides'].textContent, '20');
  assert.equal(elements['calc-result-sheets'].textContent, '40');
  assert.equal(elements['calc-result-total'].textContent, '₹120.00');

  // Behavior: Changing N-up (2-up) recalculates
  elements['calc-nup-select'].value = '2';
  elements['calc-nup-select'].trigger('change');
  // 20 pages / 2-up = 10 sides per copy, 10 sheets per copy * 2 copies = 20 sheets
  // 10 sides * 300 paise * 2 copies = 6000 paise = ₹60.00
  assert.equal(elements['calc-result-sides'].textContent, '10');
  assert.equal(elements['calc-result-sheets'].textContent, '20');
  assert.equal(elements['calc-result-total'].textContent, '₹60.00');

  // Behavior: Changing printing sides (double-sided) recalculates
  elements['calc-sides-select'].value = 'double';
  elements['calc-sides-select'].trigger('change');
  // 10 sides double-sided = 5 sheets per copy * 2 copies = 10 sheets
  // 5 full duplex sheets * 500 paise * 2 copies = 5000 paise = ₹50.00
  assert.equal(elements['calc-result-sides'].textContent, '10');
  assert.equal(elements['calc-result-sheets'].textContent, '10');
  assert.equal(elements['calc-result-total'].textContent, '₹50.00');

  // Behavior: Changing print mode (color) recalculates
  elements['calc-mode-select'].value = 'color';
  elements['calc-mode-select'].trigger('change');
  // 5 full duplex sheets * 1000 paise * 2 copies = 10000 paise = ₹100.00
  assert.equal(elements['calc-result-sides'].textContent, '10');
  assert.equal(elements['calc-result-sheets'].textContent, '10');
  assert.equal(elements['calc-result-total'].textContent, '₹100.00');

  // Behavior: Editing a tariff previews the unsaved rate immediately
  elements['price-color-duplex-input'].value = '12.00';
  elements['price-color-duplex-input'].trigger('input');
  // 5 full duplex sheets * 1200 paise * 2 copies = 12000 paise = ₹120.00
  assert.equal(elements['calc-result-sides'].textContent, '10');
  assert.equal(elements['calc-result-sheets'].textContent, '10');
  assert.equal(elements['calc-result-total'].textContent, '₹120.00');

  // Behavior: Invalid inputs display placeholders
  elements['calc-pages-input'].value = 'invalid';
  elements['calc-pages-input'].trigger('input');
  assert.equal(elements['calc-result-sides'].textContent, '--');
  assert.equal(elements['calc-result-sheets'].textContent, '--');
  assert.equal(elements['calc-result-total'].textContent, '--');

  elements['calc-pages-input'].value = '20';
  elements['calc-pages-input'].trigger('input');
  assert.equal(elements['calc-result-total'].textContent, '₹120.00');

  // Missing tariff rate preserves physical sheet count but displays placeholder for total
  elements['price-color-duplex-input'].value = '';
  elements['price-color-duplex-input'].trigger('input');
  assert.equal(elements['calc-result-sides'].textContent, '10');
  assert.equal(elements['calc-result-sheets'].textContent, '10');
  assert.equal(elements['calc-result-total'].textContent, '--');

  // Behavior: Color toggle enables/disables color inputs
  elements['pricing-color-enabled-toggle'].checked = true;
  elements['pricing-color-enabled-toggle'].trigger('change');
  assert.equal(elements['price-color-simplex-input'].disabled, false, 'Color simplex must be enabled when toggle is checked');
  assert.equal(elements['price-color-duplex-input'].disabled, false, 'Color duplex must be enabled when toggle is checked');

  elements['pricing-color-enabled-toggle'].checked = false;
  elements['pricing-color-enabled-toggle'].trigger('change');
  assert.equal(elements['price-color-simplex-input'].disabled, true, 'Color simplex must be disabled when toggle is unchecked');
  assert.equal(elements['price-color-duplex-input'].disabled, true, 'Color duplex must be disabled when toggle is unchecked');

  // Behavior: Verify zero write requests were made during all estimator interactions
  const writeCalls = networkCalls.filter(c => c.method === 'PUT' || c.method === 'POST');
  assert.equal(writeCalls.length, 0, 'No estimator interaction may issue any write requests');

  // Behavior: Only the Save Rates form submission issues an authorized PUT /api/admin/pricing
  elements['price-bw-simplex-input'].value = '3.50';
  elements['price-bw-duplex-input'].value = '5.50';
  elements['price-color-simplex-input'].value = '8.50';
  elements['price-color-duplex-input'].value = '11.00';
  elements['pricing-color-enabled-toggle'].checked = true;

  elements['pricing-form'].trigger('submit');
  // Wait a tick for async submit handler
  await new Promise(r => setTimeout(r, 10));

  const afterSubmitWrites = networkCalls.filter(c => c.method === 'PUT' || c.method === 'POST');
  assert.equal(afterSubmitWrites.length, 1, 'Only form submit may issue a pricing PUT request');
  assert.equal(afterSubmitWrites[0].endpoint, '/api/admin/pricing');
  assert.equal(afterSubmitWrites[0].method, 'PUT');
  assert.deepEqual(afterSubmitWrites[0].body, {
    bw_simplex_price_paise: 350,
    bw_duplex_price_paise: 550,
    color_simplex_price_paise: 850,
    color_duplex_price_paise: 1100,
    color_enabled: true
  });

  // Behavior: Loading an invalid pricing response fails closed and does not produce a fabricated amount
  shouldFailPricing = true;
  await controller.loadPricing();
  assert.equal(elements['price-bw-simplex-input'].value, '');
  assert.equal(elements['price-bw-simplex-input'].placeholder, '--');
  assert.equal(elements['price-bw-simplex-input'].disabled, true);
  assert.equal(elements['calc-result-total'].textContent, '--', 'Invalid pricing must fail closed to placeholder --');
});
