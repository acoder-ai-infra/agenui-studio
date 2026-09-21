interface MoneyParams {
  currency?: string;
  suffix?: string;
}

// Converts a numeric value expressed in minor units into display text.
function run(value: unknown, params?: MoneyParams): string {
  if (typeof value !== "number" || !Number.isFinite(value)) {
    throw new TypeError("format_money expects a finite number");
  }
  return (params?.currency || "¥") + (value / 100).toFixed(2) + (params?.suffix || "");
}
