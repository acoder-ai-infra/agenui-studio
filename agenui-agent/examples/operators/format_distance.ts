interface DistanceParams {
  unit?: "metric";
}

// Converts a numeric distance in meters into concise metric display text.
function run(value: unknown, _params?: DistanceParams): string {
  if (typeof value !== "number" || !Number.isFinite(value)) {
    throw new TypeError("format_distance expects a finite number");
  }
  return value < 1000 ? Math.round(value) + "m" : (value / 1000).toFixed(1) + "km";
}
