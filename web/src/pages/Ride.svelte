<script>
  import AddressInput from '../lib/AddressInput.svelte'
  import { route, navigate } from '../lib/router.svelte.js'
  import { quote, requestRide, getRide, money, chainOf, short } from '../lib/api.js'

  const id = $derived(route.query.get('id'))

  // Planning.
  let from = $state('')
  let fromPoint = $state(null)
  let to = $state('')
  let toPoint = $state(null)
  let pickup = $state(localInput(new Date(Date.now() + 15 * 60_000)))
  let riderKey = $state('')
  let fares = $state(null)
  let busy = $state(false)
  let planError = $state('')

  function localInput(d) {
    const pad = (n) => String(n).padStart(2, '0')
    return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`
  }

  const trip = () => ({ startAddress: from, endAddress: to, start: fromPoint, end: toPoint })

  async function getQuote() {
    busy = true
    planError = ''
    try {
      fares = await quote(trip())
    } catch (e) {
      planError = e.message
    } finally {
      busy = false
    }
  }

  async function order() {
    busy = true
    planError = ''
    try {
      const ride = await requestRide({
        start_address: from,
        end_address: to,
        pickup_time: new Date(pickup).toISOString(),
        cosigner_key: riderKey.trim() || undefined,
        start: fromPoint ?? undefined,
        end: toPoint ?? undefined,
      })
      if (riderKey.trim()) localStorage.setItem(`kiko:key:${ride.id}`, riderKey.trim().toLowerCase())
      navigate(`/ride?id=${ride.id}`)
    } catch (e) {
      planError = e.message
    } finally {
      busy = false
    }
  }

  // Tracking.
  let ride = $state(null)
  let payment = $state(null)
  let trackError = $state('')
  let signature = $state('')
  let paying = $state(false)
  let payError = $state('')
  let copied = $state('')

  const statuses = ['requested', 'accepted', 'paid', 'paid_out']
  const labels = { requested: 'Waiting for a driver', accepted: 'Driver on the way', paid: 'Paid', paid_out: 'Driver paid out' }
  const stage = $derived(payment ? 1 : statuses.indexOf(ride?.status ?? 'requested'))
  const req = $derived(payment?.accepts?.[0])
  const myKey = $derived(id ? localStorage.getItem(`kiko:key:${id}`) : null)

  async function refresh() {
    try {
      const r = await getRide(id)
      trackError = ''
      if (r.payment) {
        payment = r.payment
      } else {
        ride = r.ride
        payment = null
      }
    } catch (e) {
      trackError = e.message
    }
  }

  $effect(() => {
    if (!id) return
    ride = payment = null
    refresh()
    const timer = setInterval(() => ride?.status !== 'paid_out' && refresh(), 5000)
    return () => clearInterval(timer)
  })

  async function pay() {
    paying = true
    payError = ''
    try {
      const r = await getRide(id, signature)
      if (r.ride) {
        ride = r.ride
        payment = null
        signature = ''
      } else {
        payError = r.settlement?.errorReason || r.reason || 'Payment was not accepted.'
      }
    } catch (e) {
      payError = e.message
    } finally {
      paying = false
    }
  }

  async function copy(text, what) {
    await navigator.clipboard.writeText(text)
    copied = what
    setTimeout(() => (copied = ''), 1500)
  }
</script>

<section class="wrap page">
  {#if !id}
    <p class="tag">// order a ride</p>
    <h1>Where to?</h1>

    <form class="panel form" onsubmit={(e) => (e.preventDefault(), order())}>
      <AddressInput label="Pickup" placeholder="Berlin Hbf" bind:value={from} bind:point={fromPoint} />
      <AddressInput label="Drop-off" placeholder="Alexanderplatz" bind:value={to} bind:point={toPoint} />
      <label>
        Pickup time
        <input type="datetime-local" bind:value={pickup} required />
      </label>
      <details>
        <summary class="mono">Bring your own key <span>(optional, non-custodial)</span></summary>
        <label>
          Ride key
          <input class="mono" placeholder="addr_shared_vk1…  or  02…/03… (BSV)" bind:value={riderKey} />
        </label>
        <p class="hint">
          A fresh public key from your own wallet, used for this ride only: a Cardano <code>addr_shared_vk1…</code> or a
          BSV compressed public key in hex. It goes into the fare's 2-of-3 script, so Kiko can never move your fare alone.
          It also decides the chain you pay on. Without it Kiko holds the spare key, and drivers on either chain can take
          the ride.
        </p>
      </details>

      {#if fares}
        <div class="fares">
          {#each fares.fares as f}
            <div class="fare">
              <span class="mono net">{f.network}</span>
              <strong>{money(f.min, f.network)}{f.max !== f.min ? ` – ${money(f.max, f.network)}` : ''}</strong>
            </div>
          {:else}
            <p class="hint">No drivers are registered yet.</p>
          {/each}
          <span class="mono dist">{fares.distance_km.toFixed(2)} km straight line</span>
        </div>
      {/if}
      {#if planError}<p class="error">{planError}</p>{/if}

      <div class="actions">
        <button type="button" class="btn ghost" disabled={busy || !from || !to} onclick={getQuote}>Get a quote</button>
        <button class="btn" disabled={busy || !from || !to || !pickup}>Request ride →</button>
      </div>
    </form>
  {:else}
    <p class="tag">// ride <span class="mono">{short(id, 6)}</span></p>
    <h1>{payment ? 'Pay your fare' : labels[ride?.status] ?? 'Loading…'}</h1>

    <ol class="timeline mono">
      {#each statuses as s, i}
        <li class:done={i < stage} class:now={i === stage}>{s.replace('_', ' ')}</li>
      {/each}
    </ol>

    {#if trackError}<p class="error">{trackError}</p>{/if}

    {#if payment && req}
      <div class="panel card">
        <div class="amount">
          <span class="tag">{req.network}</span>
          <strong>{money(req.amount, req.network)}</strong>
          <span class="mono hint">{Number(req.amount).toLocaleString()} {req.asset}</span>
        </div>

        <span class="label">Pay to {chainOf(req.network) === 'bsv' ? '(locking script)' : '(script address)'}</span>
        <div class="copybox mono">
          <span>{req.payTo}</span>
          <button type="button" class="btn ghost" onclick={() => copy(req.payTo, 'payTo')}>{copied === 'payTo' ? 'copied' : 'copy'}</button>
        </div>
        <p class="hint">
          A new 2-of-3 multisig made only for this ride, so it has no history; that is by design.
          {#if myKey && chainOf(req.network) === 'bsv'}
            {#if req.payTo.includes(myKey)}
              <span class="ok">✓ Your ride key is in this script.</span>
            {:else}
              <span class="bad">✗ Your ride key is not in this script. Do not pay.</span>
            {/if}
          {/if}
        </p>

        <details>
          <summary class="mono">How to pay</summary>
          <ul class="hint howto">
            <li>Pay exactly the amount above with an x402 v2 (<code>exact</code>) client or wallet, using the requirements below.</li>
            {#if chainOf(req.network) === 'bsv'}
              <li>On BSV add an output locked by exactly this script, send the transaction in Extended Format (BRC-30), and use <code>&lt;txid&gt;:&lt;vout&gt;</code> of one of its inputs as the nonce.</li>
            {:else}
              <li>On Cardano send the signed Conway transaction CBOR, with <code>&lt;txHash&gt;#&lt;index&gt;</code> of one of its inputs as the nonce.</li>
            {/if}
            <li>Agents can do all of this through the MCP server at <code>/mcp</code>.</li>
          </ul>
          <button type="button" class="btn ghost" onclick={() => copy(JSON.stringify(payment, null, 2), 'req')}>
            {copied === 'req' ? 'copied' : 'Copy x402 requirements'}
          </button>
        </details>

        <label>
          Payment signature
          <textarea rows="4" placeholder="base64 PAYMENT-SIGNATURE from your wallet" bind:value={signature}></textarea>
        </label>
        {#if payError}<p class="error">{payError}</p>{/if}
        <button class="btn" disabled={paying || !signature.trim()} onclick={pay}>
          {paying ? 'Settling… this can take a moment' : 'Submit payment →'}
        </button>
      </div>
    {:else if ride}
      <div class="panel card">
        <dl class="facts">
          <dt>From</dt><dd>{ride.start_address}</dd>
          <dt>To</dt><dd>{ride.end_address}</dd>
          <dt>Pickup</dt><dd>{new Date(ride.pickup_time).toLocaleString()}</dd>
          <dt>Distance</dt><dd class="mono">{ride.distance_km.toFixed(2)} km</dd>
          {#if ride.fare}<dt>Fare</dt><dd>{money(ride.fare, ride.network)}</dd>{/if}
          <dt>Custody</dt><dd>{ride.custodial ? 'Kiko holds the spare key' : 'Your key cosigns the fare'}</dd>
          {#if ride.eta_minutes != null}<dt>Driver ETA</dt><dd>{ride.eta_minutes} min</dd>{/if}
          {#if ride.payment_tx}<dt>Payment tx</dt><dd class="mono">{short(ride.payment_tx)}</dd>{/if}
        </dl>
        {#if ride.car}
          <div class="car">
            <span class="tag">your car</span>
            <strong>{ride.car.color} {ride.car.brand} {ride.car.model}</strong>
            <span class="plate mono">{ride.car.license_plate_number}</span>
          </div>
        {:else if ride.status === 'requested'}
          <p class="mono hint waiting">searching for drivers nearby<span class="cursor"></span></p>
        {/if}
      </div>
    {/if}

    <button class="btn ghost back" onclick={() => navigate('/ride')}>← New ride</button>
  {/if}
</section>

<style>
  .page {
    padding: 2.5rem 0 1rem;
    max-width: 720px;
  }

  h1 {
    font-size: clamp(2rem, 5vw, 3rem);
    letter-spacing: -0.03em;
    margin: 0.5rem 0 1.5rem;
  }

  .form,
  .card {
    padding: 1.5rem;
    display: grid;
    gap: 1.1rem;
  }

  label input,
  label textarea {
    margin-top: 0.4rem;
    text-transform: none;
    letter-spacing: normal;
  }

  details {
    border: 1px dashed var(--line-bright);
    border-radius: 10px;
    padding: 0.75rem 0.9rem;
  }

  details[open] {
    display: grid;
    gap: 0.75rem;
  }

  summary {
    cursor: pointer;
    font-size: 0.88rem;
    color: var(--teal-pale);
  }

  summary span {
    color: var(--muted);
  }

  code {
    font-family: var(--mono);
    font-size: 0.85em;
    color: var(--teal-pale);
  }

  .fares {
    display: grid;
    gap: 0.5rem;
    padding: 1rem;
    border-radius: 10px;
    background: var(--panel-2);
    border: 1px solid var(--line);
  }

  .fare {
    display: flex;
    justify-content: space-between;
    align-items: baseline;
    gap: 1rem;
  }

  .net,
  .dist {
    font-size: 0.8rem;
    color: var(--muted);
  }

  .actions {
    display: flex;
    flex-wrap: wrap;
    gap: 0.75rem;
    justify-content: flex-end;
  }

  .timeline {
    display: flex;
    list-style: none;
    padding: 0;
    margin: 0 0 1.5rem;
    font-size: 0.75rem;
    text-transform: uppercase;
    letter-spacing: 0.08em;
  }

  .timeline li {
    flex: 1;
    padding-top: 0.6rem;
    border-top: 3px solid var(--line-bright);
    color: var(--muted);
  }

  .timeline li.done {
    border-color: var(--teal);
    color: var(--teal-soft);
  }

  .timeline li.now {
    border-color: var(--teal-pale);
    color: var(--teal-pale);
    box-shadow: 0 -6px 14px -8px var(--teal);
  }

  .amount {
    display: grid;
    gap: 0.2rem;
  }

  .amount strong {
    font-size: 2.2rem;
    letter-spacing: -0.02em;
  }

  .copybox {
    display: flex;
    gap: 0.75rem;
    align-items: center;
    background: var(--panel-2);
    border: 1px solid var(--line-bright);
    border-radius: 10px;
    padding: 0.6rem 0.6rem 0.6rem 0.9rem;
    font-size: 0.8rem;
  }

  .copybox span {
    flex: 1;
    overflow-wrap: anywhere;
    color: var(--teal-pale);
  }

  .copybox .btn {
    padding: 0.45rem 0.8rem;
    font-size: 0.78rem;
  }

  .howto {
    margin: 0;
    padding-left: 1.1rem;
  }

  .ok {
    color: var(--ok);
  }

  .bad {
    color: var(--danger);
  }

  .facts {
    display: grid;
    grid-template-columns: max-content 1fr;
    gap: 0.5rem 1.5rem;
    margin: 0;
  }

  dt {
    font: 0.78rem var(--mono);
    text-transform: uppercase;
    letter-spacing: 0.08em;
    color: var(--muted);
    padding-top: 0.15rem;
  }

  dd {
    margin: 0;
    overflow-wrap: anywhere;
  }

  .car {
    display: grid;
    gap: 0.25rem;
    padding: 1rem;
    border-radius: 10px;
    border: 1px solid var(--teal);
    background: color-mix(in srgb, var(--teal) 8%, transparent);
  }

  .car strong {
    font-size: 1.3rem;
    text-transform: capitalize;
  }

  .plate {
    justify-self: start;
    padding: 0.15rem 0.5rem;
    border-radius: 6px;
    background: var(--text);
    color: var(--bg);
    font-weight: 700;
  }

  .waiting {
    margin: 0;
  }

  .back {
    margin-top: 1.5rem;
  }
</style>
