const sharedConfig = require("@storm-software/prettier/recommended.json");

module.exports = {
  ...sharedConfig,
  overrides: [
    ...(sharedConfig.overrides ?? []),
    {
      files: "cmd/mindctl/VERSION",
      options: { parser: "yaml" }
    }
  ]
};
