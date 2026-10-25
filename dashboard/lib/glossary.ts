// Plain-language definitions shown as hover help across the dashboard and listed
// on the "How it works" page. One source so a term always means the same thing.
export const GLOSSARY = {
  paper: {
    term: "Paper trading",
    def: "Pretend trades: we work out what a $25 copy would have made at real market prices, but no money moves. Everything here is paper until live trading is turned on.",
  },
  live: {
    term: "Live trading",
    def: "Real trades with real SOL from your trading wallet. Off until you add a key and set LIVE_TRADING=true.",
  },
  delay: {
    term: "Delay (+2s, +10s, +60s)",
    def: "How long after the wallet trades we copy them. We test three delays because a slower copy usually gets a worse price.",
  },
  watching: {
    term: "Watching",
    def: "A wallet we copy on paper only, to see if it is worth copying for real. Also called the bench.",
  },
  liveWallet: {
    term: "Live wallet",
    def: "A wallet the auto-trader copies. Only wallets that stay profitable on paper for 2+ weeks should get here.",
  },
  score: {
    term: "Score",
    def: "How copyable a wallet is: our paper profit, lowered when it relies on one lucky trade or has deep losing streaks. Higher is better.",
  },
  simPnl: {
    term: "Paper PnL",
    def: "Profit or loss of our $25 paper copies of this wallet over the selected window, after fees and slippage.",
  },
  exBest: {
    term: "Without best trade",
    def: "Paper PnL with the single best trade removed. If this is negative, the wallet only looks good because of one lucky win.",
  },
  themVsUs: {
    term: "Them vs us",
    def: "Their average return per token next to ours copying them. The gap is what our delay, fees and slippage cost.",
  },
  winRate: { term: "Win rate", def: "Share of our copies that closed in profit." },
  drawdown: { term: "Max drawdown", def: "The biggest drop from a high point in our paper balance for this wallet. Smaller is safer." },
  hold: { term: "Hold time", def: "How long a position stays open, from buy to sell." },
  slippage: {
    term: "Slippage",
    def: "How much worse the price got because of the size of our trade and the time we waited. Part of every copy's cost.",
  },
  open: { term: "Open position", def: "A copy we still hold. Its PnL is an estimate at the latest price and changes until it is sold." },
  closed: { term: "Closed position", def: "A copy that has been fully sold. Its PnL is final." },
  nonsol: {
    term: "Not copyable (other coin)",
    def: "A token priced in a coin other than SOL. We can't price it, so we don't copy those trades.",
  },
  graduated: {
    term: "Graduated token",
    def: "A pump.fun token that filled its bonding curve and moved to a regular exchange. Discovery scans these for early buyers who sold well.",
  },
  discovery: {
    term: "Discovery",
    def: "Finds new wallets on its own: early buyers of tokens that ran 5x or more who also sold well. Wallets that show up repeatedly start being watched.",
  },
  killSwitch: { term: "Kill switch", def: "Stops the auto-trader from opening new positions. Sells still go through." },
  lowData: {
    term: "Low-data feed",
    def: "Other bots spam this wallet's address, so we follow only its own balance changes to save data. Its trades are still copied, about 1 second later.",
  },
} as const;

export type GlossaryKey = keyof typeof GLOSSARY;
export const help = (k: GlossaryKey) => GLOSSARY[k].def;

// Plain names for wallet statuses (the backend calls watched wallets "bench").
export const STATUS_LABEL: Record<string, string> = { live: "Live", bench: "Watching", paused: "Watching", dropped: "Removed" };
export const STATUS_HELP: Record<string, string> = {
  live: GLOSSARY.liveWallet.def,
  bench: GLOSSARY.watching.def,
  paused: GLOSSARY.watching.def,
  dropped: "No longer tracked. Re-add it to start watching again.",
};
