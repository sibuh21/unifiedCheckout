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

interface WebhookEventItem {
  id: string;
  eventType: string;
  status: string;
  eventTimestamp: string;
  receivedAt: string;
  resourceId?: string;
  orderCode?: string;
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
  const [targetOrigins, setTargetOrigins] = useState<string[]>([]);
  const [checkoutMode, setCheckoutMode] = useState<'sidebar' | 'embedded'>('embedded');
  const [copiedCard, setCopiedCard] = useState<string>('');
  const [merchantId, setMerchantId] = useState<string>('inverse_amazon_trans');

  // Webhook State
  const [webhookStatus, setWebhookStatus] = useState<string>('');
  const [webhookEvents, setWebhookEvents] = useState<WebhookEventItem[]>([]);
  const [pollingWebhook, setPollingWebhook] = useState(false);
  const [recentWebhooks, setRecentWebhooks] = useState<WebhookEventItem[]>([]);
  const [showWebhookMonitor, setShowWebhookMonitor] = useState(false);
  const [generatingKmsKey, setGeneratingKmsKey] = useState(false);
  const [kmsKeyMessage, setKmsKeyMessage] = useState('');
  const [webhookUrlInput, setWebhookUrlInput] = useState<string>('https://4tdw3h1m-8080.use.devtunnels.ms/api/webhooks/cybersource');
  const [subscribingWebhook, setSubscribingWebhook] = useState(false);
  const [subscriptionMessage, setSubscriptionMessage] = useState('');
  const [activeSubscriptions, setActiveSubscriptions] = useState<any[]>([]);

  const pollIntervalRef = useRef<any>(null);
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
      } catch (e) {
        // Suppress CyberSource internal "Cannot read properties of null (reading 'lastChild')"
      }
    }
    if (client) {
      try {
        const res = client.destroy?.();
        if (res && typeof res.catch === 'function') {
          res.catch(() => {});
        }
      } catch (e) {
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

  const fetchRecentWebhooks = useCallback(async () => {
    try {
      const res = await fetch('/api/cybersource/webhooks/events');
      if (res.ok) {
        const data = await res.json();
        setRecentWebhooks(data.events || []);
      }
    } catch (err) {
      console.warn('Could not load recent webhooks:', err);
    }
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

  const startPollingOrder = useCallback((orderIdToPoll: string) => {
    setPollingWebhook(true);
    let attempts = 0;
    if (pollIntervalRef.current) clearInterval(pollIntervalRef.current);

    pollIntervalRef.current = setInterval(async () => {
      attempts++;
      try {
        const res = await fetch(`/api/orders/${orderIdToPoll}`);
        if (res.ok) {
          const order = await res.json();
          if (order.webhookStatus) {
            setWebhookStatus(order.webhookStatus);
            setWebhookEvents(order.webhookEvents || []);
            setPollingWebhook(false);
            clearInterval(pollIntervalRef.current);
            fetchRecentWebhooks();
            return;
          }
        }
      } catch (err) {
        console.warn('Order polling attempt error:', err);
      }

      if (attempts >= 30) {
        clearInterval(pollIntervalRef.current);
        setPollingWebhook(false);
      }
    }, 2000);
  }, [fetchRecentWebhooks]);

  const launchCheckout = useCallback(async (
    mode: 'sidebar' | 'embedded' = 'embedded',
  ) => {
    setCheckoutMode(mode);
    cleanupCheckout();
    setLoading(true);
    setError('');
    setErrorDetail(null);

    const isIpHost = /^(\d{1,3}\.){3}\d{1,3}$/.test(window.location.hostname);
    const effectiveOrigin = isIpHost
      ? `https://localhost:${window.location.port || 5173}`
      : window.location.origin;

    let client: any = null;
    let checkout: any = null;

    try {
      // 1. Fetch Session Capture Context from Backend (/uc/v1/sessions)
      // Customer & Billing info is offloaded directly to Cybersource's hosted iframe (billingType: FULL)
      const res = await fetch('/api/cybersource/capture-context', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          amount: total.toFixed(2),
          targetOrigin: effectiveOrigin,
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
      setTargetOrigins(data.targetOrigins || []);

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

      // 5. Mount checkout UI
      let result: any;
      if (mode === 'embedded') {
        result = await checkout.mount({
          paymentSelection: '#payment-buttons',
          paymentScreen: '#payment-form',
        });
      } else {
        result = await checkout.mount('#payment-buttons');
      }

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
      // Transition to confirmation screen and poll for incoming signed webhook
      setSuccess(true);
      setVerifiedDetails({
        orderId: currentOrderId,
        status: 'SUBMITTED',
        result: result,
      });

      if (currentOrderId) {
        startPollingOrder(currentOrderId);
      }
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
  }, [cleanupCheckout, ensureVASLoaded, handleError, startPollingOrder, total]);

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

    // 3. Fetch Recent Webhooks
    fetchRecentWebhooks();

    return () => {
      if (pollIntervalRef.current) clearInterval(pollIntervalRef.current);
      cleanupCheckout();
    };
  }, [cleanupCheckout, fetchRecentWebhooks]);

  // Automatically launch checkout once component is mounted
  useEffect(() => {
    if (!hasInitializedRef.current) {
      hasInitializedRef.current = true;
      launchCheckout();
    }
  }, [launchCheckout]);

  const handleGenerateKmsKey = async () => {
    setGeneratingKmsKey(true);
    setKmsKeyMessage('');
    try {
      const res = await fetch('/api/cybersource/webhooks/generate-key', {
        method: 'POST',
      });
      const data = await res.json();
      if (data.success) {
        setKmsKeyMessage(`Key generated! Key ID: ${data.webhookKeyId}`);
      } else {
        setKmsKeyMessage(`Failed: ${data.error || 'Check server logs'}`);
      }
    } catch {
      setKmsKeyMessage('Connection error generating key.');
    } finally {
      setGeneratingKmsKey(false);
    }
  };

  const handleSubscribeWebhook = async () => {
    if (!webhookUrlInput) return;
    setSubscribingWebhook(true);
    setSubscriptionMessage('');
    try {
      const res = await fetch('/api/cybersource/webhooks/subscribe', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          webhookUrl: webhookUrlInput,
          productId: 'payments',
          name: 'Unified Checkout Payments Webhook',
          eventTypes: ['payments.payments.updated', 'payments.payments.created'],
        }),
      });
      const data = await res.json();
      if (res.ok && data.statusCode >= 200 && data.statusCode < 300) {
        setSubscriptionMessage('Webhook registered successfully with Cybersource!');
        handleFetchSubscriptions();
      } else {
        const msg = data.details?.message || data.details?.errorInformation?.message || JSON.stringify(data.details || data);
        setSubscriptionMessage(`Status ${res.status}: ${msg}`);
      }
    } catch (err: any) {
      setSubscriptionMessage(`Failed to subscribe: ${err.message}`);
    } finally {
      setSubscribingWebhook(false);
    }
  };

  const handleFetchSubscriptions = async () => {
    try {
      const res = await fetch('/api/cybersource/webhooks/subscriptions');
      const data = await res.json();
      if (Array.isArray(data)) {
        setActiveSubscriptions(data);
      } else if (data.subscriptions) {
        setActiveSubscriptions(data.subscriptions);
      } else if (data.details && Array.isArray(data.details)) {
        setActiveSubscriptions(data.details);
      }
    } catch (err) {
      console.warn('Could not fetch subscriptions:', err);
    }
  };

  const handleActivateSubscription = async (webhookId: string) => {
    try {
      const res = await fetch(`/api/cybersource/webhooks/activate/${webhookId}`, {
        method: 'POST',
      });
      if (res.ok) {
        setSubscriptionMessage(`Subscription ${webhookId} activated!`);
        handleFetchSubscriptions();
      } else {
        const data = await res.json();
        setSubscriptionMessage(`Failed to activate: ${data.details?.message || data.error || res.status}`);
      }
    } catch (err: any) {
      setSubscriptionMessage(`Activation error: ${err.message}`);
    }
  };


  const handleSimulateWebhook = async (status: 'SETTLED' | 'DECLINED' | 'FAILED') => {
    if (!orderId) return;
    try {
      const res = await fetch('/api/webhooks/simulate', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          orderCode: orderId,
          status: status,
          eventType: 'payments.payments.updated',
        }),
      });
      const data = await res.json();
      if (data.event) {
        setWebhookStatus(status);
        setWebhookEvents((prev) => [data.event, ...prev]);
        setPollingWebhook(false);
        fetchRecentWebhooks();
      }
    } catch (err) {
      console.error('Simulation failed:', err);
    }
  };

  // Payment Confirmation View
  if (success) {
    return (
      <div className="glass-panel success-container" style={{ maxWidth: '680px', margin: '0 auto' }}>
        <div className="success-icon">✓</div>
        <h2 style={{ fontSize: '2rem', marginBottom: '0.75rem' }}>
          {webhookStatus === 'SETTLED' ? 'Payment Completed & Settled!' :
           webhookStatus === 'AUTHORIZED' ? 'Payment Completed & Authorized!' :
           webhookStatus === 'DECLINED' || webhookStatus === 'FAILED' ? 'Payment Declined' :
           'Payment Submitted!'}
        </h2>
        <p style={{ color: 'var(--text-muted)', marginBottom: '1.5rem' }}>
          {webhookStatus ? 'Payment confirmed and verified via Cybersource signed Webhook notification.' :
           'Payment was collected directly by Cybersource Unified Checkout. Awaiting signed webhook verification...'}
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
          {(webhookEvents[0]?.resourceId || verifiedDetails?.paymentId) && (
            <div style={{ display: 'flex', justifyContent: 'space-between', marginBottom: '0.5rem' }}>
              <span style={{ color: 'var(--text-muted)' }}>Payment / Resource ID:</span>
              <span style={{ fontWeight: 600 }}>{webhookEvents[0]?.resourceId || verifiedDetails?.paymentId}</span>
            </div>
          )}
          <div style={{ display: 'flex', justifyContent: 'space-between', marginBottom: '0.5rem' }}>
            <span style={{ color: 'var(--text-muted)' }}>Payment Status:</span>
            {webhookStatus ? (
              <span style={{
                background: webhookStatus === 'SETTLED' ? 'rgba(16, 185, 129, 0.2)' : 'rgba(239, 68, 68, 0.2)',
                color: webhookStatus === 'SETTLED' ? '#10b981' : '#ef4444',
                padding: '0.2rem 0.6rem',
                borderRadius: '999px',
                fontSize: '0.8rem',
                fontWeight: 600,
              }}>
                {webhookStatus} ✓
              </span>
            ) : (
              <span style={{
                background: 'rgba(245, 158, 11, 0.2)',
                color: '#f59e0b',
                padding: '0.2rem 0.6rem',
                borderRadius: '999px',
                fontSize: '0.8rem',
                fontWeight: 600,
                display: 'inline-flex',
                alignItems: 'center',
                gap: '0.35rem',
              }}>
                <span className="spinner" style={{ width: '10px', height: '10px', borderWidth: '1.5px' }}></span>
                VERIFYING VIA WEBHOOK
              </span>
            )}
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

        {/* Real-time Webhook Notification Status */}
        <div style={{
          background: 'rgba(255, 255, 255, 0.04)',
          border: '1px solid var(--border-color)',
          borderRadius: '16px',
          padding: '1.25rem',
          textAlign: 'left',
          marginBottom: '1.5rem',
        }}>
          <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: '0.75rem' }}>
            <span style={{ fontWeight: 600, fontSize: '0.95rem' }}>Webhook Notification Status</span>
            {webhookStatus ? (
              <span style={{
                background: webhookStatus === 'SETTLED' ? 'rgba(16, 185, 129, 0.2)' : 'rgba(239, 68, 68, 0.2)',
                color: webhookStatus === 'SETTLED' ? '#10b981' : '#ef4444',
                padding: '0.25rem 0.75rem',
                borderRadius: '999px',
                fontSize: '0.8rem',
                fontWeight: 600,
              }}>
                {webhookStatus}
              </span>
            ) : pollingWebhook ? (
              <span style={{ display: 'flex', alignItems: 'center', gap: '0.4rem', color: 'var(--text-muted)', fontSize: '0.8rem' }}>
                <span className="spinner" style={{ width: '12px', height: '12px', borderWidth: '2px' }}></span>
                Listening...
              </span>
            ) : (
              <span style={{ color: 'var(--text-muted)', fontSize: '0.8rem' }}>Awaiting webhook</span>
            )}
          </div>

          {webhookStatus ? (
            <p style={{ fontSize: '0.875rem', color: '#10b981', margin: 0 }}>
              ✓ Cybersource webhook received and verified via HMAC-SHA256 signature.
            </p>
          ) : (
            <p style={{ fontSize: '0.875rem', color: 'var(--text-muted)', margin: 0 }}>
              Listening for Cybersource event notification callback...
            </p>
          )}

          {webhookEvents.length > 0 && (
            <div style={{ marginTop: '0.75rem', borderTop: '1px solid rgba(255,255,255,0.06)', paddingTop: '0.75rem' }}>
              <span style={{ fontSize: '0.75rem', color: 'var(--text-muted)' }}>Latest Event Details:</span>
              <pre style={{
                background: 'rgba(0,0,0,0.3)',
                padding: '0.5rem',
                borderRadius: '8px',
                fontSize: '0.75rem',
                overflowX: 'auto',
                marginTop: '0.25rem',
              }}>
                {JSON.stringify(webhookEvents[0], null, 2)}
              </pre>
            </div>
          )}

          <div style={{ marginTop: '1rem', display: 'flex', gap: '0.5rem', flexWrap: 'wrap' }}>
            <button
              type="button"
              onClick={() => handleSimulateWebhook('SETTLED')}
              style={{
                background: 'rgba(16, 185, 129, 0.15)',
                color: '#10b981',
                border: '1px solid rgba(16, 185, 129, 0.3)',
                padding: '0.4rem 0.75rem',
                borderRadius: '8px',
                fontSize: '0.75rem',
                cursor: 'pointer',
              }}
            >
              Simulate Webhook (SETTLED)
            </button>
            <button
              type="button"
              onClick={() => handleSimulateWebhook('DECLINED')}
              style={{
                background: 'rgba(239, 68, 68, 0.15)',
                color: '#ef4444',
                border: '1px solid rgba(239, 68, 68, 0.3)',
                padding: '0.4rem 0.75rem',
                borderRadius: '8px',
                fontSize: '0.75rem',
                cursor: 'pointer',
              }}
            >
              Simulate Webhook (DECLINED)
            </button>
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
        <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: '1.25rem' }}>
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
              Cybersource Unified Checkout v1 Integration
            </p>
          </div>
          <button
            type="button"
            onClick={() => setShowWebhookMonitor(!showWebhookMonitor)}
            style={{
              background: 'rgba(255,255,255,0.06)',
              border: '1px solid var(--border-color)',
              color: 'var(--text-muted)',
              fontSize: '0.75rem',
              padding: '0.4rem 0.75rem',
              borderRadius: '8px',
              cursor: 'pointer',
            }}
          >
            {showWebhookMonitor ? 'Hide Monitor' : 'Webhook Monitor'}
          </button>
        </div>

        {/* Origin & Target Context Info */}
        <div style={{
          background: 'rgba(255,255,255,0.03)',
          border: '1px solid var(--border-color)',
          borderRadius: '10px',
          padding: '0.75rem',
          fontSize: '0.75rem',
          color: 'var(--text-muted)',
          marginBottom: '1.25rem',
        }}>
          <div><strong>Current Page Origin:</strong> <code>{window.location.origin}</code></div>
          {targetOrigins.length > 0 && (
            <div style={{ marginTop: '0.25rem' }}>
              <strong>Configured Target Origins:</strong> {targetOrigins.join(', ')}
            </div>
          )}
        </div>

        {/* IP Address Warning Banner */}
        {/^(\d{1,3}\.){3}\d{1,3}$/.test(window.location.hostname) && (
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
              ⚠️ Direct IP Address Access Detected ({window.location.hostname})
            </div>
            <p style={{ color: 'var(--text-main)', marginBottom: '0.5rem' }}>
              Cybersource Unified Checkout strictly requires a <strong>Fully Qualified Domain Name (FQDN)</strong>.
              Direct IP addresses (such as <code>172.22.0.1</code> or <code>192.168.1.162</code>) are rejected by Cybersource.
            </p>
            <div>
              👉 Please access the checkout via localhost:
              <a
                href={`https://localhost:${window.location.port || 5173}/`}
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
                Switch to https://localhost:{window.location.port || 5173}/
              </a>
            </div>
          </div>
        )}

        {/* Webhook Monitor Drawer */}
        {showWebhookMonitor && (
          <div style={{
            background: 'rgba(0,0,0,0.4)',
            border: '1px solid var(--border-color)',
            borderRadius: '12px',
            padding: '1rem',
            marginBottom: '1.5rem',
            fontSize: '0.8rem',
          }}>
            <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: '0.75rem' }}>
              <div style={{ fontWeight: 600, color: 'var(--primary)' }}>
                🔔 Cybersource Notification Service (Webhooks)
              </div>
              <span style={{ fontSize: '0.7rem', color: 'var(--text-muted)' }}>
                Receiver: <code>/api/webhooks/cybersource</code>
              </span>
            </div>

            {/* 1. Public Webhook URL & Registration */}
            <div style={{ marginBottom: '0.75rem', padding: '0.75rem', background: 'rgba(255,255,255,0.03)', borderRadius: '8px' }}>
              <div style={{ fontSize: '0.75rem', color: 'var(--text-muted)', marginBottom: '0.35rem' }}>
                Public Webhook Destination URL (must be HTTPS tunnel):
              </div>
              <div style={{ display: 'flex', gap: '0.5rem', marginBottom: '0.5rem' }}>
                <input
                  type="text"
                  value={webhookUrlInput}
                  onChange={(e) => setWebhookUrlInput(e.target.value)}
                  placeholder="https://<tunnel-url>/api/webhooks/cybersource"
                  style={{
                    flex: 1,
                    background: 'rgba(0,0,0,0.5)',
                    border: '1px solid var(--border-color)',
                    borderRadius: '6px',
                    padding: '0.4rem 0.6rem',
                    color: '#fff',
                    fontSize: '0.75rem',
                  }}
                />
                <button
                  type="button"
                  onClick={handleSubscribeWebhook}
                  disabled={subscribingWebhook}
                  style={{
                    background: '#10b981',
                    color: '#fff',
                    border: 'none',
                    padding: '0.4rem 0.75rem',
                    borderRadius: '6px',
                    fontSize: '0.75rem',
                    fontWeight: 600,
                    cursor: 'pointer',
                    whiteSpace: 'nowrap',
                  }}
                >
                  {subscribingWebhook ? 'Registering...' : '📡 2. Register Webhook'}
                </button>
              </div>
              {subscriptionMessage && (
                <div style={{ fontSize: '0.75rem', color: subscriptionMessage.includes('successfully') ? '#10b981' : '#f59e0b' }}>
                  {subscriptionMessage}
                </div>
              )}
            </div>

            {/* 2. KMS Webhook Secret Key */}
            <div style={{ marginBottom: '0.75rem', padding: '0.75rem', background: 'rgba(255,255,255,0.03)', borderRadius: '8px' }}>
              <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
                <div>
                  <div style={{ fontSize: '0.75rem', fontWeight: 600 }}>1. KMS Digital Signature Key</div>
                  <div style={{ fontSize: '0.7rem', color: 'var(--text-muted)' }}>Required for HMAC-SHA256 signature verification</div>
                </div>
                <button
                  type="button"
                  onClick={handleGenerateKmsKey}
                  disabled={generatingKmsKey}
                  style={{
                    background: 'var(--primary)',
                    color: '#fff',
                    border: 'none',
                    padding: '0.35rem 0.75rem',
                    borderRadius: '6px',
                    fontSize: '0.75rem',
                    cursor: 'pointer',
                  }}
                >
                  {generatingKmsKey ? 'Calling KMS...' : '⚡ Generate KMS Key'}
                </button>
              </div>
              {kmsKeyMessage && (
                <div style={{ marginTop: '0.5rem', fontSize: '0.75rem', color: '#10b981' }}>
                  {kmsKeyMessage}
                </div>
              )}
            </div>

            {/* 3. Subscriptions Explorer */}
            <div style={{ marginBottom: '0.75rem', padding: '0.75rem', background: 'rgba(255,255,255,0.03)', borderRadius: '8px' }}>
              <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: '0.35rem' }}>
                <span style={{ fontSize: '0.75rem', color: 'var(--text-muted)' }}>Active Cybersource Subscriptions:</span>
                <button
                  type="button"
                  onClick={handleFetchSubscriptions}
                  style={{
                    background: 'rgba(255,255,255,0.08)',
                    color: '#fff',
                    border: 'none',
                    padding: '0.25rem 0.6rem',
                    borderRadius: '4px',
                    fontSize: '0.7rem',
                    cursor: 'pointer',
                  }}
                >
                  ↻ Refresh
                </button>
              </div>
              {activeSubscriptions.length === 0 ? (
                <div style={{ color: 'var(--text-muted)', fontSize: '0.7rem' }}>Click Refresh to view active subscriptions from Cybersource.</div>
              ) : (
                <div style={{ maxHeight: '100px', overflowY: 'auto', fontSize: '0.7rem' }}>
                  {activeSubscriptions.map((sub: any, idx: number) => (
                    <div key={idx} style={{ padding: '0.35rem 0', borderBottom: '1px solid rgba(255,255,255,0.05)', display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
                      <div>
                        <strong>{sub.name || sub.webhookId || 'Subscription'}</strong>: {sub.webhookUrl || sub.targetUrl}
                        <span style={{ marginLeft: '0.4rem', color: sub.status === 'ACTIVE' ? '#10b981' : '#f59e0b' }}>
                          ({sub.status || 'ACTIVE'})
                        </span>
                      </div>
                      {sub.status !== 'ACTIVE' && (
                        <button
                          type="button"
                          onClick={() => handleActivateSubscription(sub.webhookId)}
                          style={{
                            background: '#10b981',
                            color: '#fff',
                            border: 'none',
                            padding: '0.2rem 0.5rem',
                            borderRadius: '4px',
                            fontSize: '0.65rem',
                            fontWeight: 600,
                            cursor: 'pointer',
                          }}
                        >
                          ⚡ Activate
                        </button>
                      )}
                    </div>
                  ))}
                </div>
              )}
            </div>

            {/* 4. Recent Received Webhooks */}
            <div style={{ fontWeight: 600, marginBottom: '0.25rem', display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
              <span>Recent Received Events ({recentWebhooks.length}):</span>
              <button
                type="button"
                onClick={fetchRecentWebhooks}
                style={{
                  background: 'none',
                  border: 'none',
                  color: 'var(--primary)',
                  fontSize: '0.7rem',
                  cursor: 'pointer',
                }}
              >
                ↻ Refresh Events
              </button>
            </div>
            {recentWebhooks.length === 0 ? (
              <div style={{ color: 'var(--text-muted)', fontSize: '0.75rem' }}>No events received yet. Complete a payment to see notifications live!</div>
            ) : (
              <div style={{ maxHeight: '120px', overflowY: 'auto' }}>
                {recentWebhooks.map((evt, idx) => (
                  <div key={idx} style={{ padding: '0.35rem 0', borderBottom: '1px solid rgba(255,255,255,0.05)', display: 'flex', justifyContent: 'space-between' }}>
                    <span>{evt.eventType} - <strong>{evt.status}</strong> ({evt.orderCode || 'N/A'})</span>
                    <span style={{ color: 'var(--text-muted)' }}>{new Date(evt.receivedAt).toLocaleTimeString()}</span>
                  </div>
                ))}
              </div>
            )}
          </div>
        )}

        {/* Display Mode Switcher */}
        <div style={{ display: 'flex', gap: '0.5rem', marginBottom: '1.25rem', alignItems: 'center' }}>
          <span style={{ fontSize: '0.8rem', color: 'var(--text-muted)' }}>Display Mode:</span>
          <button
            type="button"
            onClick={() => launchCheckout('embedded')}
            style={{
              background: checkoutMode === 'embedded' ? 'var(--primary)' : 'rgba(255,255,255,0.06)',
              color: '#fff',
              border: 'none',
              padding: '0.3rem 0.75rem',
              borderRadius: '6px',
              fontSize: '0.75rem',
              cursor: 'pointer',
              fontWeight: checkoutMode === 'embedded' ? 600 : 400,
            }}
          >
            Embedded Mode (Default)
          </button>
          <button
            type="button"
            onClick={() => launchCheckout('sidebar')}
            style={{
              background: checkoutMode === 'sidebar' ? 'var(--primary)' : 'rgba(255,255,255,0.06)',
              color: '#fff',
              border: 'none',
              padding: '0.3rem 0.75rem',
              borderRadius: '6px',
              fontSize: '0.75rem',
              cursor: 'pointer',
              fontWeight: checkoutMode === 'sidebar' ? 600 : 400,
            }}
          >
            Sidebar Mode
          </button>
        </div>

        {/* Sandbox Test Cards Assistant */}
        <div style={{
          background: 'rgba(255, 255, 255, 0.02)',
          border: '1px solid var(--border-color)',
          borderRadius: '10px',
          padding: '0.75rem 1rem',
          marginBottom: '1.25rem',
          fontSize: '0.78rem',
        }}>
          <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: '0.5rem' }}>
            <span style={{ fontWeight: 600, color: 'var(--text-main)' }}>💳 Cybersource Sandbox Test Cards</span>
            {copiedCard && <span style={{ color: '#10b981', fontSize: '0.72rem' }}>Copied {copiedCard}!</span>}
          </div>
          <div style={{ display: 'flex', flexDirection: 'column', gap: '0.4rem' }}>
            <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
              <span style={{ color: 'var(--text-muted)' }}>Visa (Default Test): <code>4000 0000 0000 0002</code> (12/28, CVV 123)</span>
              <button
                type="button"
                onClick={() => {
                  navigator.clipboard.writeText('4000000000000002');
                  setCopiedCard('Visa 4000...0002');
                  setTimeout(() => setCopiedCard(''), 2000);
                }}
                style={{ background: 'rgba(255,255,255,0.08)', color: '#fff', border: 'none', padding: '0.2rem 0.5rem', borderRadius: '4px', cursor: 'pointer', fontSize: '0.7rem' }}
              >
                Copy
              </button>
            </div>
            <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
              <span style={{ color: 'var(--text-muted)' }}>Visa (Alternative): <code>4111 1111 1111 1111</code> (12/28, CVV 123)</span>
              <button
                type="button"
                onClick={() => {
                  navigator.clipboard.writeText('4111111111111111');
                  setCopiedCard('Visa 4111...1111');
                  setTimeout(() => setCopiedCard(''), 2000);
                }}
                style={{ background: 'rgba(255,255,255,0.08)', color: '#fff', border: 'none', padding: '0.2rem 0.5rem', borderRadius: '4px', cursor: 'pointer', fontSize: '0.7rem' }}
              >
                Copy
              </button>
            </div>
            <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
              <span style={{ color: 'var(--text-muted)' }}>Mastercard: <code>5105 1051 0510 5100</code> (12/28, CVV 123)</span>
              <button
                type="button"
                onClick={() => {
                  navigator.clipboard.writeText('5105105105105100');
                  setCopiedCard('Mastercard 5105...5100');
                  setTimeout(() => setCopiedCard(''), 2000);
                }}
                style={{ background: 'rgba(255,255,255,0.08)', color: '#fff', border: 'none', padding: '0.2rem 0.5rem', borderRadius: '4px', cursor: 'pointer', fontSize: '0.7rem' }}
              >
                Copy
              </button>
            </div>
          </div>
        </div>

        {/* Error Notification */}
        {error && (
          <div className="error-message" style={{ marginBottom: '1.25rem' }}>
            <div style={{ fontWeight: 600 }}>{error}</div>
            {errorDetail?.reason && (
              <div style={{ fontSize: '0.75rem', marginTop: '0.25rem', opacity: 0.9 }}>
                Reason: {errorDetail.reason}
              </div>
            )}

            {/* Diagnostic helper for COMPLETE_ERROR or Processor error */}
            {errorDetail?.reason === 'COMPLETE_ERROR' && (
              <div style={{
                marginTop: '0.75rem',
                padding: '0.75rem',
                background: 'rgba(0, 0, 0, 0.25)',
                borderRadius: '8px',
                border: '1px solid rgba(239, 68, 68, 0.3)',
                fontSize: '0.8rem',
                lineHeight: 1.4,
              }}>
                <div style={{ color: '#fca5a5', fontWeight: 600, marginBottom: '0.25rem' }}>
                  ℹ️ Troubleshooting:
                </div>
                <div style={{ color: 'var(--text-muted)', marginBottom: '0.5rem' }}>
                  Make sure the test card number and expiration date are valid. Recommended test card: <code>4000 0000 0000 0002</code>.
                </div>
              </div>
            )}

            <button
              type="button"
              onClick={() => launchCheckout(checkoutMode)}
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
              ↻ Retry Checkout Initialization
            </button>
          </div>
        )}

        {/* Cybersource Native Collection Banner */}
        <div style={{
          background: 'rgba(59, 130, 246, 0.08)',
          border: '1px solid rgba(59, 130, 246, 0.25)',
          borderRadius: '14px',
          padding: '1rem 1.25rem',
          marginBottom: '1.5rem',
          display: 'flex',
          alignItems: 'center',
          gap: '0.85rem',
        }}>
          <span style={{ fontSize: '1.5rem' }}>🛡️</span>
          <div style={{ flex: 1 }}>
            <div style={{ fontSize: '0.9rem', fontWeight: 600, color: '#93c5fd' }}>
              Cybersource Hosted Checkout (Full Billing Collection)
            </div>
            <div style={{ fontSize: '0.78rem', color: 'var(--text-muted)', marginTop: '0.15rem' }}>
              Customer info, billing address, and card details are securely collected and validated directly inside Cybersource's hosted iframe.
            </div>
          </div>
          <button
            type="button"
            onClick={() => launchCheckout(checkoutMode)}
            style={{
              background: 'rgba(255,255,255,0.08)',
              color: '#fff',
              border: '1px solid var(--border-color)',
              padding: '0.35rem 0.75rem',
              borderRadius: '8px',
              fontSize: '0.75rem',
              cursor: 'pointer',
              whiteSpace: 'nowrap',
            }}
          >
            ↻ Reload Iframe
          </button>
        </div>

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

          {/* Dedicated Mount Containers for Unified Checkout */}
          <div
            id="payment-buttons"
            style={{ minHeight: '45px', marginBottom: '0.75rem' }}
          />
          <div
            id="payment-form"
            style={{ minHeight: '280px' }}
          />

          {/* Legacy/Alternative IDs for compatibility */}
          <div id="buttons-container" />
          <div id="form-container" />

          {!loading && !checkoutMounted && !error && (
            <div style={{ textAlign: 'center', padding: '1.5rem 0' }}>
              <button
                type="button"
                className="btn-primary"
                onClick={() => launchCheckout(checkoutMode)}
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
