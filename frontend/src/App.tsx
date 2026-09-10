import { useState, useEffect, useRef, useCallback } from 'react'
import './index.css'

declare global {
  interface Window {
    VAS?: {
      UnifiedCheckout: (sessionJWT: string) => Promise<{
        createCheckout: (options?: { autoProcessing?: boolean }) => Promise<{
          mount: (selectorOrOptions?: string | { paymentSelection?: string; paymentScreen?: string }) => Promise<any>;
          unmount: () => void;
          destroy: () => void;
          complete: (token?: string) => Promise<any>;
        }>;
        destroy: () => void;
      }>;
    };
  }
}

interface CartItem {
  id: string;
  name: string;
  price: number;
  quantity: number;
  image: string;
}

function App() {
  const [cart, setCart] = useState<CartItem[]>([
    {
      id: 'prod_1',
      name: 'Sample Item',
      price: 21.00,
      quantity: 1,
      image: 'https://images.unsplash.com/photo-1505740420928-5e560c06d30e?w=800&q=80',
    },
  ]);
  const [loading, setLoading] = useState(false);
  const [checkoutMounted, setCheckoutMounted] = useState(false);
  const [error, setError] = useState('');
  const [errorDetail, setErrorDetail] = useState<{ reason?: string; message?: string } | null>(null);
  const [success, setSuccess] = useState(false);
  const [orderId, setOrderId] = useState('');
  const [verifiedDetails, setVerifiedDetails] = useState<any>(null);
  const [sessionJwt, setSessionJwt] = useState('');
  const [merchantId, setMerchantId] = useState<string>('inverse_amazon_trans');

  const activeCheckoutRef = useRef<any>(null);
  const activeClientRef = useRef<any>(null);
  const hasInitializedRef = useRef(false);

  const subtotal = cart.reduce((acc, item) => acc + item.price * item.quantity, 0);
  const tax = 0.00;
  const total = subtotal + tax;

  const safeDestroyCheckout = (checkout: any, client: any) => {
    if (checkout) {
      try {
        const res = checkout.destroy?.();
        if (res && typeof res.catch === 'function') {
          res.catch(() => {});
        }
      } catch {
        // Suppress CyberSource internal "Cannot read properties of null (reading 'lastChild')"
      }
    }
    if (client) {
      try {
        const res = client.destroy?.();
        if (res && typeof res.catch === 'function') {
          res.catch(() => {});
        }
      } catch {
        // Suppress CyberSource client destroy error
      }
    }
  };

  const cleanupCheckout = useCallback(() => {
    const checkout = activeCheckoutRef.current;
    const client = activeClientRef.current;
    activeCheckoutRef.current = null;
    activeClientRef.current = null;
    safeDestroyCheckout(checkout, client);
    const btn = document.getElementById('payment-buttons');
    if (btn) btn.innerHTML = '';
    const form = document.getElementById('payment-form');
    if (form) form.innerHTML = '';
    setCheckoutMounted(false);
  }, []);

  const ensureVASLoaded = useCallback(async (): Promise<any> => {
    if (window.VAS?.UnifiedCheckout) {
      return window.VAS;
    }

    return new Promise((resolve, reject) => {
      let script = document.getElementById('cybersource-unified-checkout') as HTMLScriptElement;
      if (!script) {
        script = document.createElement('script');
        script.id = 'cybersource-unified-checkout';
        script.src = 'https://apitest.cybersource.com/uc/v1/assets/1.0.0/UnifiedCheckout.js';
        script.async = true;
        document.head.appendChild(script);
      }

      const checkInterval = setInterval(() => {
        if (window.VAS?.UnifiedCheckout) {
          clearInterval(checkInterval);
          resolve(window.VAS);
        }
      }, 100);

      script.onload = () => {
        if (window.VAS?.UnifiedCheckout) {
          clearInterval(checkInterval);
          resolve(window.VAS);
        }
      };

      script.onerror = () => {
        clearInterval(checkInterval);
        reject(new Error('Failed to load Cybersource UnifiedCheckout.js library'));
      };

      // Timeout after 8 seconds
      setTimeout(() => {
        clearInterval(checkInterval);
        if (window.VAS?.UnifiedCheckout) {
          resolve(window.VAS);
        } else {
          reject(new Error('Timed out waiting for VAS.UnifiedCheckout to initialize'));
        }
      }, 8000);
    });
  }, []);

  const handleError = useCallback((reason?: string, message?: string) => {
    console.error('UnifiedCheckoutError caught:', { reason, message });
    setErrorDetail({ reason, message });
    setError(`UnifiedCheckoutError: ${reason ? `[${reason}] ` : ''}${message || 'Payment failed'}`);
  }, []);

  const launchCheckout = useCallback(async () => {
    cleanupCheckout();
    setLoading(true);
    setError('');
    setErrorDetail(null);

    let client: any = null;
    let checkout: any = null;

    try {
      // 1. Fetch Session Capture Context from Backend (/uc/v1/sessions)
      // Strictly using https://localhost:5173 as target origin
      const res = await fetch('/api/cybersource/capture-context', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          amount: total.toFixed(2),
          targetOrigin: 'https://localhost:5173',
        }),
      });

      if (!res.ok) {
        let errMsg = `Server returned HTTP ${res.status}`;
        try {
          const text = await res.text();
          try {
            const errData = JSON.parse(text);
            errMsg = errData.details || errData.error || errMsg;
          } catch {
            if (text) errMsg = text;
          }
        } catch {
          // Keep default errMsg
        }
        throw new Error(errMsg);
      }

      const data = await res.json();
      const sessionJWT = data.jwt;
      const currentOrderId = data.orderId;
      if (!sessionJWT) {
        throw new Error('No session JWT returned from server');
      }

      setSessionJwt(sessionJWT);
      setOrderId(currentOrderId);

      // 2. Ensure VAS library is ready
      const vas = await ensureVASLoaded();
      if (!vas?.UnifiedCheckout) {
        throw new Error('VAS.UnifiedCheckout function not found');
      }

      // 3. Initialize SDK by calling VAS.UnifiedCheckout(sessionJWT)
      client = await vas.UnifiedCheckout(sessionJWT);
      activeClientRef.current = client;

      // 4. Create checkout instance (autoProcessing enables direct browser payment collection)
      checkout = await client.createCheckout({ autoProcessing: true });
      activeCheckoutRef.current = checkout;

      setLoading(false);
      setCheckoutMounted(true);

      // 5. Always Mount in Sidebar Mode
      const result = await checkout.mount('#payment-buttons');

      console.log('Unified Checkout payment submission result:', result);

      // Clean up SDK instances and clear mount container BEFORE triggering state transition
      safeDestroyCheckout(checkout, client);
      activeCheckoutRef.current = null;
      activeClientRef.current = null;
      const btnEl = document.getElementById('payment-buttons');
      if (btnEl) btnEl.innerHTML = '';
      const formEl = document.getElementById('payment-form');
      if (formEl) formEl.innerHTML = '';

      // 6. Payment submitted directly to Cybersource!
      setSuccess(true);
      setVerifiedDetails({
        orderId: currentOrderId,
        status: 'COMPLETED',
        result: result,
      });
    } catch (err: any) {
      console.error('Checkout failed:', err);
      if (err.name === 'UnifiedCheckoutError') {
        handleError(err.reason, err.message);
      } else {
        setError(err.message || 'Payment interface could not be launched');
      }
    } finally {
      safeDestroyCheckout(checkout, client);
      activeCheckoutRef.current = null;
      activeClientRef.current = null;
      setLoading(false);
      setCheckoutMounted(false);
    }
  }, [cleanupCheckout, ensureVASLoaded, handleError, total]);

  useEffect(() => {
    // 1. Fetch Cart from backend
    fetch('/api/cart')
      .then((res) => res.json())
      .then((data) => {
        if (data.items && data.items.length > 0) {
          setCart(data.items);
        }
      })
      .catch((err) => console.warn('Could not fetch cart, using default cart:', err));

    // 2. Fetch Active Merchant Configuration
    fetch('/api/health')
      .then((res) => res.json())
      .then((data) => {
        if (data.merchantId) {
          setMerchantId(data.merchantId);
        }
      })
      .catch(() => {});

    return () => {
      cleanupCheckout();
    };
  }, [cleanupCheckout]);

  // Automatically launch checkout once component is mounted
  useEffect(() => {
    if (!hasInitializedRef.current) {
      hasInitializedRef.current = true;
      launchCheckout();
    }
  }, [launchCheckout]);

  // Payment Confirmation View
  if (success) {
    return (
      <div className="glass-panel success-container" style={{ maxWidth: '680px', margin: '0 auto' }}>
        <div className="success-icon">✓</div>
        <h2 style={{ fontSize: '2rem', marginBottom: '0.75rem' }}>
          Payment Completed Successfully!
        </h2>
        <p style={{ color: 'var(--text-muted)', marginBottom: '1.5rem' }}>
          Your transaction was securely processed and completed directly by Cybersource Unified Checkout.
        </p>

        <div style={{
          background: 'rgba(255, 255, 255, 0.04)',
          border: '1px solid var(--border-color)',
          borderRadius: '16px',
          padding: '1.25rem',
          textAlign: 'left',
          marginBottom: '1.5rem',
        }}>
          <div style={{ display: 'flex', justifyContent: 'space-between', marginBottom: '0.5rem' }}>
            <span style={{ color: 'var(--text-muted)' }}>Order ID:</span>
            <span style={{ fontWeight: 600, color: 'var(--primary)' }}>{orderId}</span>
          </div>
          {verifiedDetails?.result?.id && (
            <div style={{ display: 'flex', justifyContent: 'space-between', marginBottom: '0.5rem' }}>
              <span style={{ color: 'var(--text-muted)' }}>Transaction ID:</span>
              <span style={{ fontWeight: 600 }}>{verifiedDetails.result.id}</span>
            </div>
          )}
          <div style={{ display: 'flex', justifyContent: 'space-between', marginBottom: '0.5rem' }}>
            <span style={{ color: 'var(--text-muted)' }}>Payment Status:</span>
            <span style={{
              background: 'rgba(16, 185, 129, 0.2)',
              color: '#10b981',
              padding: '0.2rem 0.6rem',
              borderRadius: '999px',
              fontSize: '0.8rem',
              fontWeight: 600,
            }}>
              COMPLETED ✓
            </span>
          </div>
          <div style={{ display: 'flex', justifyContent: 'space-between', marginBottom: '0.5rem' }}>
            <span style={{ color: 'var(--text-muted)' }}>Customer & Billing:</span>
            <span style={{ fontWeight: 500, color: '#10b981' }}>Collected securely by Cybersource</span>
          </div>
          <div style={{ display: 'flex', justifyContent: 'space-between' }}>
            <span style={{ color: 'var(--text-muted)' }}>Total Amount:</span>
            <span style={{ fontWeight: 600 }}>${total.toFixed(2)} USD</span>
          </div>
        </div>

        <button
          className="btn-primary"
          style={{ width: 'auto', display: 'inline-flex', padding: '0.875rem 2rem' }}
          onClick={() => window.location.reload()}
        >
          Return to Shop
        </button>
      </div>
    );
  }

  return (
    <div className="glass-panel checkout-container">
      {/* Checkout Left Section */}
      <div className="form-section">
        <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: '1.5rem' }}>
          <div>
            <div style={{ display: 'flex', alignItems: 'center', gap: '0.6rem' }}>
              <h1 style={{ margin: 0 }}>Unified Checkout</h1>
              {merchantId && (
                <span style={{
                  background: 'rgba(59, 130, 246, 0.15)',
                  color: '#60a5fa',
                  border: '1px solid rgba(59, 130, 246, 0.3)',
                  padding: '0.2rem 0.6rem',
                  borderRadius: '6px',
                  fontSize: '0.75rem',
                  fontWeight: 600,
                }}>
                  MID: {merchantId}
                </span>
              )}
            </div>
            <p style={{ color: 'var(--text-muted)', fontSize: '0.875rem', marginTop: '0.25rem' }}>
              Cybersource Unified Checkout Integration
            </p>
          </div>
        </div>

        {/* Target Origin Warning Banner */}
        {window.location.origin !== 'https://localhost:5173' && (
          <div style={{
            background: 'rgba(245, 158, 11, 0.12)',
            border: '1px solid rgba(245, 158, 11, 0.3)',
            borderRadius: '12px',
            padding: '1rem',
            marginBottom: '1.25rem',
            fontSize: '0.85rem',
            lineHeight: 1.5,
          }}>
            <div style={{ fontWeight: 600, color: '#f59e0b', marginBottom: '0.25rem' }}>
              ⚠️ Access via https://localhost:5173/ Required
            </div>
            <p style={{ color: 'var(--text-main)', marginBottom: '0.5rem' }}>
              Cybersource Unified Checkout target origin is configured strictly for <strong>https://localhost:5173/</strong>.
              Accessing via <code>{window.location.origin}</code> will cause Cybersource to reject the session.
            </p>
            <div>
              👉 Please access the checkout via:
              <a
                href="https://localhost:5173/"
                style={{
                  display: 'inline-block',
                  marginLeft: '0.5rem',
                  background: 'var(--primary)',
                  color: '#fff',
                  padding: '0.35rem 0.85rem',
                  borderRadius: '6px',
                  textDecoration: 'none',
                  fontWeight: 600,
                  fontSize: '0.8rem',
                }}
              >
                Switch to https://localhost:5173/
              </a>
            </div>
          </div>
        )}

        {/* Error Notification */}
        {error && (
          <div className="error-message" style={{ marginBottom: '1.25rem' }}>
            <div style={{ fontWeight: 600 }}>{error}</div>
            {errorDetail?.reason && (
              <div style={{ fontSize: '0.75rem', marginTop: '0.25rem', opacity: 0.9 }}>
                Reason: {errorDetail.reason}
              </div>
            )}
            <button
              type="button"
              onClick={() => launchCheckout()}
              style={{
                marginTop: '0.75rem',
                background: 'rgba(255,255,255,0.15)',
                color: '#fff',
                border: 'none',
                padding: '0.35rem 0.75rem',
                borderRadius: '6px',
                fontSize: '0.75rem',
                cursor: 'pointer',
              }}
            >
              ↻ Retry Checkout
            </button>
          </div>
        )}

        {/* Unified Checkout Payment UI Area */}
        <div style={{ marginTop: '1rem' }}>
          <h2 style={{ marginBottom: '1rem' }}>Payment Method</h2>

          {loading && !checkoutMounted && (
            <div style={{ textAlign: 'center', padding: '2.5rem 0' }}>
              <div className="spinner" style={{ margin: '0 auto 0.75rem auto' }}></div>
              <p style={{ color: 'var(--text-muted)', fontSize: '0.875rem' }}>
                Connecting to Cybersource Unified Checkout...
              </p>
            </div>
          )}

          {/* Dedicated Mount Containers for Unified Checkout Sidebar Mode */}
          <div
            id="payment-buttons"
            style={{ minHeight: '48px', marginBottom: '0.75rem' }}
          />
          <div id="payment-form" style={{ display: 'none' }} />
          <div id="buttons-container" style={{ display: 'none' }} />
          <div id="form-container" style={{ display: 'none' }} />

          {!loading && !checkoutMounted && !error && (
            <div style={{ textAlign: 'center', padding: '1.5rem 0' }}>
              <button
                type="button"
                className="btn-primary"
                onClick={() => launchCheckout()}
                style={{ width: 'auto', display: 'inline-flex', padding: '0.75rem 2rem' }}
              >
                Launch Unified Checkout
              </button>
            </div>
          )}

          <div style={{ textAlign: 'center', marginTop: '2rem', fontSize: '0.75rem', color: 'var(--text-muted)' }}>
            Secured by CyberSource Unified Checkout
          </div>
        </div>
      </div>

      {/* Order Summary Right Section */}
      <div className="summary-section">
        <h2>Order Summary</h2>
        <div style={{ marginBottom: '2rem' }}>
          {cart.map((item) => (
            <div key={item.id} className="cart-item">
              <img src={item.image} alt={item.name} className="item-image" />
              <div className="item-details">
                <div className="item-name">{item.name}</div>
                <div className="item-price">Qty: {item.quantity}</div>
              </div>
              <div className="item-price" style={{ fontWeight: 500, color: 'var(--text-main)' }}>
                ${item.price.toFixed(2)}
              </div>
            </div>
          ))}
        </div>

        <div className="summary-row">
          <span>Subtotal</span>
          <span>${subtotal.toFixed(2)}</span>
        </div>
        <div className="summary-row">
          <span>Taxes</span>
          <span>$0.00</span>
        </div>
        <div className="summary-row">
          <span>Shipping</span>
          <span>Free</span>
        </div>

        <div className="summary-total">
          <span>Total</span>
          <span>${total.toFixed(2)} USD</span>
        </div>

        {sessionJwt && (
          <div style={{ marginTop: '1.5rem', padding: '0.75rem', background: 'rgba(255,255,255,0.03)', borderRadius: '8px' }}>
            <div style={{ fontSize: '0.75rem', color: 'var(--text-muted)', marginBottom: '0.25rem' }}>
              Session Status:
            </div>
            <div style={{ fontSize: '0.75rem', color: '#10b981', display: 'flex', alignItems: 'center', gap: '0.4rem' }}>
              <span>●</span> Active Capture Context
            </div>
          </div>
        )}
      </div>
    </div>
  );
}

export default App;
