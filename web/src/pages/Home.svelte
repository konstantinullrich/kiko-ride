<script>
  import { link } from '../lib/router.svelte.js'

  const session = [
    ['$', 'kiko ride --from "Berlin Hbf" --to "Alexanderplatz"'],
    ['quote', '3.01 km · 3.505 ADA | 35,000 sat'],
    ['accepted', 'driver 4 min away · fare locked'],
    ['402', 'pay to fresh 2-of-3 script 5221…53ae'],
    ['settled', 'tx 8e46…4118 ✓  car: red Tesla 3'],
  ]
  let shown = $state(1)
  $effect(() => {
    const timer = setInterval(() => (shown = shown >= session.length ? 1 : shown + 1), 1400)
    return () => clearInterval(timer)
  })

  const features = [
    ['01', 'A fresh address per ride', 'Every fare lands in its own 2-of-3 multisig, never reused. Your rides cannot be linked on chain.'],
    ['02', 'Your key, your fare', 'Bring a key for the ride and Kiko can never move the money alone. Skip it and Kiko holds the spare.'],
    ['03', 'Two chains', 'Pay in ADA on Cardano or in satoshis on Bitcoin SV. Drivers choose their chain, the fare follows.'],
    ['04', 'Agent-native', 'Fares are plain x402 payments, and the rider API is an MCP server at /mcp. Let your agent book the ride.'],
  ]

  const steps = [
    ['Request', 'Pick a pickup and a drop-off. Get a quote in ADA and BSV.'],
    ['Match', 'A nearby driver accepts. The fare and payment script are fixed.'],
    ['Pay', 'Answer the HTTP 402 with a signed transaction. It settles on chain.'],
    ['Ride', 'You see the car. The driver is paid out by cosigning with Kiko.'],
  ]
</script>

<section class="hero wrap">
  <div class="copy">
    <p class="tag">// peer-to-peer ride hailing</p>
    <h1>Rides, paid <span class="glow">on chain</span>.</h1>
    <p class="lead">
      Order a ride and pay the driver directly in ADA or BSV. Every fare is locked by three keys: yours, the
      driver's and Kiko's. Nobody moves it alone.
    </p>
    <p class="first mono">
      <span>◆</span> The first decentralized, self-custodial, agentic ride-hailing app.
    </p>
    <div class="actions">
      <a class="btn" href="/ride" onclick={link}>Order a ride →</a>
      <a class="btn ghost" href="#how">How it works</a>
    </div>
  </div>
  <div class="visual">
    <img src="/kiko.png" alt="Kiko hummingbird" width="300" height="300" />
  </div>
</section>

<section class="wrap">
  <div class="terminal panel mono" aria-label="Example ride session">
    <div class="bar"><span></span><span></span><span></span><em>rider@kiko:~</em></div>
    {#each session.slice(0, shown) as [key, value], i}
      <p class:prompt={key === '$'}>
        <b>{key === '$' ? '$' : key.padEnd(9)}</b><span class:cursor={i === shown - 1}>{value}</span>
      </p>
    {/each}
  </div>
</section>

<section class="wrap why">
  <div class="panel why-card">
    <p class="tag">// a first</p>
    <h2>Decentralized. Self-custodial. Agentic.</h2>
    <p>
      Kiko is the first decentralized, self-custodial, agentic ride-hailing app, and the distributed multisig is what
      makes that possible. A ride-hailing platform normally holds your money: you pay the company, the company pays
      the driver. Kiko never does. Every fare is locked in its own 2-of-3 multisig shared by the rider, the driver and
      Kiko, so the money moves only when two of the three agree. No single party, Kiko included, can take it.
    </p>
    <div class="pillars mono">
      <span><b>decentralized</b> custody split across three independent keys</span>
      <span><b>self-custodial</b> your ride key cosigns your fare</span>
      <span><b>agentic</b> x402 payments and an MCP server for AI agents</span>
    </div>
  </div>
</section>

<section class="wrap features">
  {#each features as [n, title, text]}
    <article class="panel">
      <span class="mono n">{n}</span>
      <h3>{title}</h3>
      <p>{text}</p>
    </article>
  {/each}
</section>

<section id="how" class="wrap how">
  <p class="tag">// how it works</p>
  <h2>Four steps, zero trust required.</h2>
  <ol>
    {#each steps as [title, text], i}
      <li class="panel">
        <span class="mono">0x0{i + 1}</span>
        <h3>{title}</h3>
        <p>{text}</p>
      </li>
    {/each}
  </ol>
  <div class="cta panel">
    <p class="mono">&gt; ready when you are<span class="cursor"></span></p>
    <a class="btn" href="/ride" onclick={link}>Order a ride →</a>
  </div>
</section>

<style>
  .hero {
    display: grid;
    grid-template-columns: 1.3fr 1fr;
    align-items: center;
    gap: 2rem;
    padding: 4rem 0 3rem;
  }

  h1 {
    font-size: clamp(2.4rem, 6vw, 4.2rem);
    line-height: 1.05;
    letter-spacing: -0.03em;
    margin: 0.75rem 0 1rem;
  }

  h2 {
    font-size: clamp(1.6rem, 3.5vw, 2.3rem);
    letter-spacing: -0.02em;
    margin: 0.5rem 0 1.5rem;
  }

  .glow {
    color: var(--teal-soft);
    text-shadow: 0 0 24px rgba(16, 160, 144, 0.55);
  }

  .lead {
    font-size: 1.15rem;
    color: var(--muted);
    max-width: 34rem;
  }

  .actions {
    display: flex;
    flex-wrap: wrap;
    gap: 0.75rem;
    margin-top: 1.75rem;
  }

  .first {
    display: inline-flex;
    gap: 0.6rem;
    align-items: baseline;
    margin: 1.25rem 0 0;
    padding: 0.55rem 0.9rem;
    border: 1px solid var(--teal);
    border-radius: 10px;
    background: color-mix(in srgb, var(--teal) 10%, transparent);
    color: var(--teal-pale);
    font-size: 0.88rem;
    box-shadow: 0 0 24px -8px var(--teal);
  }

  .first span {
    color: var(--teal);
  }

  .why {
    margin-top: 3rem;
  }

  .why-card {
    padding: 2rem;
    border-color: var(--teal);
    background: linear-gradient(135deg, color-mix(in srgb, var(--teal) 12%, var(--panel)), var(--panel) 60%);
  }

  .why-card > p:not(.tag) {
    color: var(--muted);
    max-width: 52rem;
    margin: 0;
  }

  .pillars {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(240px, 1fr));
    gap: 0.75rem;
    margin-top: 1.5rem;
    font-size: 0.82rem;
    color: var(--muted);
  }

  .pillars span {
    padding: 0.75rem 0.9rem;
    border: 1px solid var(--line-bright);
    border-radius: 10px;
  }

  .pillars b {
    display: block;
    color: var(--teal-pale);
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: 0.08em;
    margin-bottom: 0.2rem;
  }

  .visual {
    display: grid;
    place-items: center;
  }

  .visual img {
    width: min(300px, 70vw);
    height: auto;
    filter: drop-shadow(0 0 40px rgba(16, 160, 144, 0.45));
    animation: hover 5s ease-in-out infinite;
  }

  @keyframes hover {
    50% {
      transform: translateY(-10px) rotate(-2deg);
    }
  }

  .terminal {
    padding: 0 1.25rem 1.25rem;
    font-size: 0.9rem;
    min-height: 15.5rem;
    overflow-x: auto;
  }

  .terminal .bar {
    display: flex;
    gap: 0.4rem;
    align-items: center;
    padding: 0.9rem 0;
    margin-bottom: 0.5rem;
    border-bottom: 1px solid var(--line);
  }

  .bar span {
    width: 10px;
    height: 10px;
    border-radius: 50%;
    background: var(--line-bright);
  }

  .bar em {
    margin-left: auto;
    font-style: normal;
    color: var(--muted);
    font-size: 0.78rem;
  }

  .terminal p {
    margin: 0.35rem 0;
    white-space: pre;
  }

  .terminal b {
    color: var(--teal-soft);
    font-weight: 500;
    margin-right: 0.75rem;
  }

  .terminal .prompt b {
    color: var(--ok);
  }

  .features {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(230px, 1fr));
    gap: 1rem;
    margin-top: 3rem;
  }

  article {
    padding: 1.4rem;
    transition: border-color 0.2s, transform 0.2s;
  }

  article:hover,
  li:hover {
    border-color: var(--teal);
    transform: translateY(-2px);
  }

  .n {
    color: var(--teal);
    font-size: 0.8rem;
  }

  h3 {
    margin: 0.4rem 0;
    font-size: 1.1rem;
  }

  article p,
  li p {
    margin: 0;
    color: var(--muted);
    font-size: 0.95rem;
  }

  .how {
    margin-top: 5rem;
  }

  ol {
    list-style: none;
    padding: 0;
    margin: 0;
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(220px, 1fr));
    gap: 1rem;
  }

  li {
    padding: 1.4rem;
    transition: border-color 0.2s, transform 0.2s;
  }

  li span {
    color: var(--teal);
    font-size: 0.8rem;
  }

  .cta {
    margin-top: 1.5rem;
    padding: 1.5rem;
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    justify-content: space-between;
    gap: 1rem;
  }

  .cta p {
    margin: 0;
    color: var(--teal-pale);
  }

  @media (max-width: 780px) {
    .hero {
      grid-template-columns: 1fr;
      padding-top: 2rem;
    }

    .visual {
      order: -1;
    }

    .visual img {
      width: 180px;
    }
  }
</style>
