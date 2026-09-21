interface PickParams {
  index?: number;
  matchField?: string;
  matchValue?: unknown;
  extractField?: string;
}

function fail(code: "PICK_NOT_FOUND" | "PICK_NOT_UNIQUE", message: string): never {
  throw { code, message };
}

function run(value: unknown, params?: PickParams): unknown {
  if (!Array.isArray(value)) {
    throw new TypeError("list.pick expects an array");
  }
  let picked: unknown;
  if (params?.matchField) {
    if (params.index !== undefined || !Object.prototype.hasOwnProperty.call(params, "matchValue")) {
      throw new TypeError("list.pick requires exactly one selector");
    }
    const matches = value.filter(
      (item) => item !== null && typeof item === "object" &&
        (item as Record<string, unknown>)[params.matchField!] === params.matchValue,
    );
    if (matches.length === 0) fail("PICK_NOT_FOUND", "list.pick found no match");
    if (matches.length !== 1) fail("PICK_NOT_UNIQUE", "list.pick found multiple matches");
    picked = matches[0];
  } else {
    const index = params?.index ?? 0;
    if (!Number.isInteger(index) || index < 0 || index >= value.length) {
      fail("PICK_NOT_FOUND", "list.pick index is out of range");
    }
    picked = value[index];
  }
  if (!params?.extractField) return picked;
  if (picked === null || typeof picked !== "object" || !(params.extractField in picked)) {
    fail("PICK_NOT_FOUND", "list.pick extract field is missing");
  }
  return (picked as Record<string, unknown>)[params.extractField];
}
