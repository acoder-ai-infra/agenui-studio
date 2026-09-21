interface MapParams {
  field: string;
}

function run(value: unknown, params?: MapParams): unknown[] {
  if (!Array.isArray(value) || !params?.field) {
    throw new TypeError("list.map expects an array and field");
  }
  return value.map((item) => {
    if (item === null || typeof item !== "object" || !(params.field in item)) {
      throw new TypeError("list.map field is missing");
    }
    return (item as Record<string, unknown>)[params.field];
  });
}
