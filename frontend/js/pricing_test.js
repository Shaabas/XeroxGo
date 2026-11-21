// @ts-check
const test = require('node:test');
const assert = require('node:assert/strict');
const { calculateDocPrice, fetchPricing, setPricingConfig, formatRupees } = require('./portal.js');

test('Frontend Pricing — Fails closed when pricing is uninitialized', () => {
  setPricingConfig(null);
  const price = calculateDocPrice(3, {
    mode: 'B&W',
    sides: 'Single-sided',
    copies: 1,
    pagesPerSheet: '1'
  });
  assert.equal(price, 0, 'Price must be 0 when pricing has not loaded');
});

test('Frontend Pricing — Calculates canonical tariff matrix accurately', () => {
  // Canonical development rates:
  // BW Simplex: 300, BW Duplex: 500
  // Color Simplex: 800, Color Duplex: 1000
  setPricingConfig({
    bw_simplex_paise: 300,
    bw_duplex_paise: 500,
    color_simplex_paise: 800,
    color_duplex_paise: 1000,
    color_enabled: true
  });

  // B&W Simplex: 1..5 pages
  assert.equal(calculateDocPrice(1, { mode: 'B&W', sides: 'Single-sided', copies: 1, pagesPerSheet: '1' }), 300);
  assert.equal(calculateDocPrice(2, { mode: 'B&W', sides: 'Single-sided', copies: 1, pagesPerSheet: '1' }), 600);
  assert.equal(calculateDocPrice(3, { mode: 'B&W', sides: 'Single-sided', copies: 1, pagesPerSheet: '1' }), 900);
  assert.equal(calculateDocPrice(4, { mode: 'B&W', sides: 'Single-sided', copies: 1, pagesPerSheet: '1' }), 1200);
  assert.equal(calculateDocPrice(5, { mode: 'B&W', sides: 'Single-sided', copies: 1, pagesPerSheet: '1' }), 1500);

  // B&W Duplex: 1..5 pages (floor(p/2)*500 + (p%2)*300)
  assert.equal(calculateDocPrice(1, { mode: 'B&W', sides: 'Double-sided', copies: 1, pagesPerSheet: '1' }), 300);
  assert.equal(calculateDocPrice(2, { mode: 'B&W', sides: 'Double-sided', copies: 1, pagesPerSheet: '1' }), 500);
  assert.equal(calculateDocPrice(3, { mode: 'B&W', sides: 'Double-sided', copies: 1, pagesPerSheet: '1' }), 800);
  assert.equal(calculateDocPrice(4, { mode: 'B&W', sides: 'Double-sided', copies: 1, pagesPerSheet: '1' }), 1000);
  assert.equal(calculateDocPrice(5, { mode: 'B&W', sides: 'Double-sided', copies: 1, pagesPerSheet: '1' }), 1300);

  // Color Simplex: 1..5 pages
  assert.equal(calculateDocPrice(1, { mode: 'Color', sides: 'Single-sided', copies: 1, pagesPerSheet: '1' }), 800);
  assert.equal(calculateDocPrice(2, { mode: 'Color', sides: 'Single-sided', copies: 1, pagesPerSheet: '1' }), 1600);
  assert.equal(calculateDocPrice(3, { mode: 'Color', sides: 'Single-sided', copies: 1, pagesPerSheet: '1' }), 2400);
  assert.equal(calculateDocPrice(4, { mode: 'Color', sides: 'Single-sided', copies: 1, pagesPerSheet: '1' }), 3200);
  assert.equal(calculateDocPrice(5, { mode: 'Color', sides: 'Single-sided', copies: 1, pagesPerSheet: '1' }), 4000);

  // Color Duplex: 1..5 pages (floor(p/2)*1000 + (p%2)*800)
  assert.equal(calculateDocPrice(1, { mode: 'Color', sides: 'Double-sided', copies: 1, pagesPerSheet: '1' }), 800);
  assert.equal(calculateDocPrice(2, { mode: 'Color', sides: 'Double-sided', copies: 1, pagesPerSheet: '1' }), 1000);
  assert.equal(calculateDocPrice(3, { mode: 'Color', sides: 'Double-sided', copies: 1, pagesPerSheet: '1' }), 1800);
  assert.equal(calculateDocPrice(4, { mode: 'Color', sides: 'Double-sided', copies: 1, pagesPerSheet: '1' }), 2000);
  assert.equal(calculateDocPrice(5, { mode: 'Color', sides: 'Double-sided', copies: 1, pagesPerSheet: '1' }), 2800);

  // Copies multiplier
  assert.equal(calculateDocPrice(3, { mode: 'B&W', sides: 'Double-sided', copies: 3, pagesPerSheet: '1' }), 800 * 3); // 2400

  // N-up: 4 pages on 2-up = 2 imposed pages
  assert.equal(calculateDocPrice(4, { mode: 'B&W', sides: 'Double-sided', copies: 1, pagesPerSheet: '2' }), 500); // 2 imposed pages = 1 sheet @ 500
});

test('Frontend Pricing — Color disabled hardware gracefully falls back to B&W rates', () => {
  setPricingConfig({
    bw_simplex_paise: 300,
    bw_duplex_paise: 500,
    color_simplex_paise: 800,
    color_duplex_paise: 1000,
    color_enabled: false
  });

  // Mode is Color, but hardware color_enabled is false -> billed as B&W
  const simplexPrice = calculateDocPrice(2, { mode: 'Color', sides: 'Single-sided', copies: 1, pagesPerSheet: '1' });
  assert.equal(simplexPrice, 600, 'Should fall back to B&W Simplex rate (2 * 300)');

  const duplexPrice = calculateDocPrice(3, { mode: 'Color', sides: 'Double-sided', copies: 1, pagesPerSheet: '1' });
  assert.equal(duplexPrice, 800, 'Should fall back to B&W Duplex rate (1*500 + 1*300)');
});

test('Frontend Pricing — fetchPricing loads rates from API', async () => {
  global.fetch = async (url) => {
    if (url === '/api/pricing') {
      return {
        ok: true,
        json: async () => ({
          bw_simplex_paise: 250,
          bw_duplex_paise: 400,
          color_simplex_paise: 700,
          color_duplex_paise: 950,
          color_enabled: true
        })
      };
    }
    throw new Error('Not found: ' + url);
  };

  await fetchPricing();

  const price = calculateDocPrice(3, { mode: 'B&W', sides: 'Double-sided', copies: 1, pagesPerSheet: '1' });
  // 1 sheet @ 400 + 1 odd page @ 250 = 650
  assert.equal(price, 650, 'Price should be calculated using fetched rates');
  assert.equal(formatRupees(price), '₹6.50');
});
