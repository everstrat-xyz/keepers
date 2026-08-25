/** @type {import('ts-jest').JestConfigWithTsJest} */
module.exports = {
  preset: "ts-jest",
  testEnvironment: "node",
  roots: ["<rootDir>/src", "<rootDir>"],
  // index.ts imports the Gelato SDK, which is only meaningful inside the W3F
  // runtime; its decision logic lives in src/ and is tested directly.
  testPathIgnorePatterns: ["/node_modules/", "<rootDir>/index.ts$"],
};
