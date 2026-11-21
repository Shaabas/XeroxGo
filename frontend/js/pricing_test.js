// @ts-check
const test = require('node:test');
const assert = require('node:assert/strict');

// Set up mock DOM before loading portal.js to enable full UI cart lifecycle testing
const mockElements = {};
function createMockElement(id) {
  return {
    id,
    classList: {
      add: () => {},
      remove: () => {},
      contains: () => false,
      toggle: () => false
    },
    addEventListener: () => {},
    appendChild: () => {},
    querySelector: () => null,
    querySelectorAll: () => [],
    children: [],
    dataset: { devMode: 'true', kioskId: 'kiosk-test' },
    style: {},
    innerHTML: '',
    textContent: '',
    disabled: false
  };
}

global.document = {
  readyState: 'complete',
  addEventListener: () => {},
  getElementById: (id) => mockElements[id] || (mockElements[id] = createMockElement(id)),
  querySelector: (sel) => mockElements[sel] || (mockElements[sel] = createMockElement(sel)),
  querySelectorAll: () => [],
  createElement: (tag) => createMockElement(tag)
};

global.window = {
  __PORTAL_TEST__: true,
  location: { pathname: '/k/kiosk-test', search: '' },
  scrollTo: () => {},
  addEventListener: () => {},
  document: global.document
};

// Default initial fetch to fail so pricing starts uninitialized
global.fetch = async () => ({ ok: false });

require('./portal.js');
const portal = window.__portal;

const { calculateDocPrice, fetchPricing, setPricingConfig, formatRupees } = portal;

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
    bw_simplex_price_paise: 300,
    bw_duplex_price_paise: 500,
    color_simplex_price_paise: 800,
    color_duplex_price_paise: 1000,
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
    bw_simplex_price_paise: 300,
    bw_duplex_price_paise: 500,
    color_simplex_price_paise: 800,
    color_duplex_price_paise: 1000,
    color_enabled: false
  });

  // Mode is Color, but hardware color_enabled is false -> billed as B&W
  const simplexPrice = calculateDocPrice(2, { mode: 'Color', sides: 'Single-sided', copies: 1, pagesPerSheet: '1' });
  assert.equal(simplexPrice, 600, 'Should fall back to B&W Simplex rate (2 * 300)');

  const duplexPrice = calculateDocPrice(3, { mode: 'Color', sides: 'Double-sided', copies: 1, pagesPerSheet: '1' });
  assert.equal(duplexPrice, 800, 'Should fall back to B&W Duplex rate (1*500 + 1*300)');
});

test('Frontend Pricing — fetchPricing loads canonical rates from API', async () => {
  global.fetch = async (url) => {
    if (url === '/api/pricing') {
      return {
        ok: true,
        json: async () => ({
          bw_simplex_price_paise: 250,
          bw_duplex_price_paise: 400,
          color_simplex_price_paise: 700,
          color_duplex_price_paise: 950,
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

test('Frontend Pricing — UI Cart: Successful pricing fetch enables Place Order and displays correct amount', async () => {
  const btnReviewOrder = mockElements['btn-review-order'];
  const cartTotalAmount = mockElements['cart-total-amount'];

  // Add a valid ready document: 3 pages B&W Double-sided
  portal.setCartItems([{
    id: 'doc-ready-1',
    name: 'document.pdf',
    size: 2048,
    status: 'ready',
    totalPages: 3,
    settings: {
      mode: 'B&W',
      sides: 'Double-sided',
      copies: 1,
      pagesPerSheet: '1'
    }
  }]);

  // Mock canonical response: BW Simplex 300, BW Duplex 500 -> 3 pages = 1 sheet (500) + 1 side (300) = 800 paise = ₹8.00
  global.fetch = async (url) => {
    if (url === '/api/pricing') {
      return {
        ok: true,
        json: async () => ({
          bw_simplex_price_paise: 300,
          bw_duplex_price_paise: 500,
          color_simplex_price_paise: 800,
          color_duplex_price_paise: 1000,
          color_enabled: true
        })
      };
    }
    throw new Error('Unexpected URL: ' + url);
  };

  await fetchPricing();

  assert.equal(btnReviewOrder.disabled, false, 'Place Order button must be enabled when pricing loads for ready doc');
  assert.equal(cartTotalAmount.textContent, '₹8.00', 'Cart total amount must reflect calculated order price');
});

test('Frontend Pricing — UI Cart: Malformed or missing rates keep checkout disabled', () => {
  const btnReviewOrder = mockElements['btn-review-order'];
  const cartTotalAmount = mockElements['cart-total-amount'];

  portal.setCartItems([{
    id: 'doc-ready-2',
    name: 'document2.pdf',
    size: 1024,
    status: 'ready',
    totalPages: 2,
    settings: {
      mode: 'B&W',
      sides: 'Single-sided',
      copies: 1,
      pagesPerSheet: '1'
    }
  }]);

  // Test case A: null / unconfigured
  setPricingConfig(null);
  assert.equal(btnReviewOrder.disabled, true, 'Checkout must be disabled when pricing is null');
  assert.equal(cartTotalAmount.textContent, '--', 'Cart total must display placeholder -- when pricing is uninitialized');

  // Test case B: missing color_duplex_price_paise
  setPricingConfig({
    bw_simplex_price_paise: 300,
    bw_duplex_price_paise: 500,
    color_simplex_price_paise: 800
  });
  assert.equal(btnReviewOrder.disabled, true, 'Checkout must be disabled when rate field is missing');
  assert.equal(cartTotalAmount.textContent, '--', 'Cart total must display placeholder -- on incomplete rates');

  // Test case C: non-positive / zero rate
  setPricingConfig({
    bw_simplex_price_paise: 0,
    bw_duplex_price_paise: 500,
    color_simplex_price_paise: 800,
    color_duplex_price_paise: 1000
  });
  assert.equal(btnReviewOrder.disabled, true, 'Checkout must be disabled when rate is <= 0');
  assert.equal(cartTotalAmount.textContent, '--', 'Cart total must display placeholder -- on zero rate');

  // Test case D: negative rate
  setPricingConfig({
    bw_simplex_price_paise: 300,
    bw_duplex_price_paise: -500,
    color_simplex_price_paise: 800,
    color_duplex_price_paise: 1000
  });
  assert.equal(btnReviewOrder.disabled, true, 'Checkout must be disabled when rate is negative');
  assert.equal(cartTotalAmount.textContent, '--', 'Cart total must display placeholder -- on negative rate');

  // Test case E: non-number string rate
  setPricingConfig({
    bw_simplex_price_paise: '300',
    bw_duplex_price_paise: 500,
    color_simplex_price_paise: 800,
    color_duplex_price_paise: 1000
  });
  assert.equal(btnReviewOrder.disabled, true, 'Checkout must be disabled when rate is string');
  assert.equal(cartTotalAmount.textContent, '--', 'Cart total must display placeholder -- on string rate');
});
