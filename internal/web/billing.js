(function(root) {
  const esc=value=>String(value??'').replace(/[&<>"']/g,ch=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[ch]));
  const money=value=>Number.isFinite(value) ? '$'+value.toFixed(2) : 'unavailable';
  const quantity=value=>Number.isFinite(value) ? value.toLocaleString('en-US',{maximumFractionDigits:2}) : 'unavailable';
  function render(card) {
    const b=card.billing;
    const stamp=card.fetchedAt ? new Date(card.fetchedAt).toLocaleString('en-US',{timeZone:'America/New_York'})+' ET' : 'pending';
    if (!b) return `<section class="ubilling" data-billing-key="${esc(card.key)}"><h3>${esc(card.label)}</h3><p class="ubilling-error">${esc(card.error || 'Billing not reporting')}</p><p class="ubilling-note">Last attempt ${esc(stamp)}</p></section>`;
    const attention=b.attention;
    const copilot=(b.products || []).filter(p=>String(p.product).toLowerCase()==='copilot');
    const creditAhead=b.creditNetUsd>copilot.reduce((n,p)=>n+p.netUsd,0) || b.creditGrossUsd>copilot.reduce((n,p)=>n+p.grossUsd,0);
    const stale=Date.now()-Date.parse(card.fetchedAt || "")>180000;
    const status=card.error ? "Refresh failed · cached" : attention ? "Attention" : stale ? "Stale reading" : "No reported spend alert";
    return `<section class="ubilling" data-billing-key="${esc(card.key)}"><h3>${esc(card.label)} <span class="${attention || card.error?'ubilling-error':'ubilling-note'}">${status}</span></h3>
      <div class="ubilling-lead"><span><b>${quantity(b.credits)}</b> Copilot AI credits used</span><span>${quantity(b.projectedCredits)} projected at month end</span><span><b>${money(b.netUsd)}</b> net in usage summary</span><span>${money(b.projectedNetUsd)} projected summary net</span></div>
      <p class="ubilling-note">${esc(b.month)} UTC billing period · updated ${esc(stamp)} · projection assumes the month-to-date pace continues</p>
      <p class="ubilling-note">AI credits: ${money(b.creditGrossUsd)} gross · ${money(b.creditNetUsd)} net. Usage summary gross: ${money(b.grossUsd)}. Totals include every SKU in the usage summary, including subscription SKUs reported by GitHub. AI-credit amounts are shown separately.</p>
      ${attention?'<p class="ubilling-error">Attention: reported spend without an applicable product stop budget, or Copilot net spend.</p>':''}
      ${creditAhead?`<p class="${b.creditNetUsd>copilot.reduce((n,p)=>n+p.netUsd,0)?'ubilling-error':'ubilling-note'}">AI-credit report is ahead of the usage summary: Copilot ${money(b.creditNetUsd)} net / ${money(b.creditGrossUsd)} gross. Summary totals and projections retain the summary API values until GitHub updates them.</p>`:''}
      <h4>Product spend from usage summary</h4><div class="ubilling-scroll" role="region" aria-label="GitHub spend by product" tabindex="0"><table><thead><tr><th scope="col">Product</th><th scope="col">Net</th><th scope="col">Gross</th><th scope="col">Product stop budget</th></tr></thead><tbody>${b.products?.length ? b.products.map(p=>`<tr><th scope="row">${esc(p.product)}</th><td>${money(p.netUsd)}</td><td>${money(p.grossUsd)}</td><td class="${p.attention?'ubilling-error':''}">${p.hasStopBudget?'Yes':'None'}${p.attention?' · attention':''}</td></tr>`).join('') : '<tr><td colspan="4">No metered usage this month</td></tr>'}</tbody></table></div>
      <h4>Budgets</h4><div class="ubilling-scroll" role="region" aria-label="GitHub configured budgets" tabindex="0"><table><thead><tr><th scope="col">Product / SKU</th><th scope="col">Amount</th><th scope="col">Stops usage</th><th scope="col">Scope / type</th></tr></thead><tbody>${b.budgets?.length ? b.budgets.map(v=>`<tr><th scope="row">${esc(v.products.join(', '))}</th><td>${money(v.amount)}</td><td>${v.stopsUsage?'Yes':'No'}</td><td>${esc(v.scope)} · ${esc(v.type)}${v.entity?' · '+esc(v.entity):''}</td></tr>`).join('') : '<tr><td colspan="4">No configured budgets</td></tr>'}</tbody></table></div>
      <p class="ubilling-note">Copilot paid-usage policy is not exposed by the API. Any Copilot net spend requires attention. A missing Copilot budget does not prove paid usage is enabled.</p>
      ${card.error?`<p class="ubilling-error">${esc(card.error)}</p>`:''}
    </section>`;
  }
  root.BillingDashboard={render,money};if(typeof module!=='undefined')module.exports=root.BillingDashboard;
})(typeof window==='undefined'?globalThis:window);
