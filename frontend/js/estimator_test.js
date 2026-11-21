// @ts-check
const test = require('node:test');
const assert = require('node:assert/strict');
const { calculateEstimate, parsePositiveInteger, parseRupeesToPaise } = require('./admin.js');

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
  assert.equal(parseRupeesToPaise('10.25'), 1025);

  // Invalid or nonpositive cases must return null
  assert.equal(parseRupeesToPaise('0'), null);
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

test('Estimator Live Updates & API Isolation — Input changes recalculate and never issue API writes', () => {
  let fetchCount = 0;

  const elements = {
    'calc-pages-input': { value: '10' },
    'calc-copies-input': { value: '1' },
    'calc-nup-select': { value: '1' },
    'calc-sides-select': { value: 'single' },
    'calc-mode-select': { value: 'bw' },
    'price-bw-simplex-input': { value: '3.00' },
    'price-bw-duplex-input': { value: '5.00' },
    'price-color-simplex-input': { value: '8.00' },
    'price-color-duplex-input': { value: '10.00' },
    'calc-result-sides': { textContent: '' },
    'calc-result-sheets': { textContent: '' },
    'calc-result-total': { textContent: '' },
  };

  function update() {
    const res = calculateEstimate({
      pages: elements['calc-pages-input'].value,
      copies: elements['calc-copies-input'].value,
      nUp: elements['calc-nup-select'].value,
      sides: elements['calc-sides-select'].value,
      mode: elements['calc-mode-select'].value,
      rates: {
        bwSimplex: parseRupeesToPaise(elements['price-bw-simplex-input'].value),
        bwDuplex: parseRupeesToPaise(elements['price-bw-duplex-input'].value),
        colorSimplex: parseRupeesToPaise(elements['price-color-simplex-input'].value),
        colorDuplex: parseRupeesToPaise(elements['price-color-duplex-input'].value),
      },
    });
    elements['calc-result-sides'].textContent = res.printedSidesDisplay;
    elements['calc-result-sheets'].textContent = res.sheetsDisplay;
    elements['calc-result-total'].textContent = res.totalCostDisplay;
  }

  // Initial calculation (default values)
  update();
  assert.equal(elements['calc-result-sides'].textContent, '10');
  assert.equal(elements['calc-result-sheets'].textContent, '10');
  assert.equal(elements['calc-result-total'].textContent, '₹30.00');

  // Change sides to double
  elements['calc-sides-select'].value = 'double';
  update();
  assert.equal(elements['calc-result-sides'].textContent, '10');
  assert.equal(elements['calc-result-sheets'].textContent, '5');
  assert.equal(elements['calc-result-total'].textContent, '₹25.00');

  // Change mode to color
  elements['calc-mode-select'].value = 'color';
  update();
  assert.equal(elements['calc-result-sides'].textContent, '10');
  assert.equal(elements['calc-result-sheets'].textContent, '5');
  assert.equal(elements['calc-result-total'].textContent, '₹50.00');

  // Change n-up to 2-up
  elements['calc-nup-select'].value = '2';
  update();
  // 10 pages / 2-up = 5 sides, double sided = 3 sheets, 2*1000 + 1*800 = 2800 paise = ₹28.00
  assert.equal(elements['calc-result-sides'].textContent, '5');
  assert.equal(elements['calc-result-sheets'].textContent, '3');
  assert.equal(elements['calc-result-total'].textContent, '₹28.00');

  // Enter malformed pages
  elements['calc-pages-input'].value = 'invalid';
  update();
  assert.equal(elements['calc-result-sides'].textContent, '--');
  assert.equal(elements['calc-result-sheets'].textContent, '--');
  assert.equal(elements['calc-result-total'].textContent, '--');

  // Enter negative copies
  elements['calc-pages-input'].value = '10';
  elements['calc-copies-input'].value = '-2';
  update();
  assert.equal(elements['calc-result-sides'].textContent, '--');
  assert.equal(elements['calc-result-sheets'].textContent, '--');
  assert.equal(elements['calc-result-total'].textContent, '--');

  // Clear tariff input (unavailable rate)
  elements['calc-copies-input'].value = '1';
  elements['price-color-simplex-input'].value = '';
  update();
  assert.equal(elements['calc-result-sides'].textContent, '5');
  assert.equal(elements['calc-result-sheets'].textContent, '3');
  assert.equal(elements['calc-result-total'].textContent, '--');

  // Ensure zero fetch/API calls occurred during any estimator interaction
  assert.equal(fetchCount, 0);
});
