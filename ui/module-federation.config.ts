// Shared singletons must match the platform shell
// (specs/003-application-gateway/contracts/federation.md).
// A production remote never carries fallback copies of the singletons: the
// shell always provides them (import: false), which keeps every module bundle
// to its own code. `vite` dev keeps the local copies for standalone use.
const hostOnly = process.env.NODE_ENV === 'production' ? { import: false as const } : {}
export const shared = {
  vue: { singleton: true, requiredVersion: '^3.5.0', ...hostOnly },
  'vue-router': { singleton: true, requiredVersion: '^5.0.0', ...hostOnly },
  pinia: { singleton: true, requiredVersion: '^4.0.0', ...hostOnly },
  '@casl/ability': { singleton: true, requiredVersion: '^7.0.0', ...hostOnly },
  '@casl/vue': { singleton: true, requiredVersion: '^3.0.0', ...hostOnly },
  zod: { singleton: true, requiredVersion: '^4.0.0', strictVersion: true, ...hostOnly },
  '@go-tangra/ui': { singleton: true, requiredVersion: '^4.0.0', strictVersion: true, ...hostOnly },
  '@go-tangra/ui/forms': { singleton: true, requiredVersion: '^4.0.0', strictVersion: true, ...hostOnly },
  '@go-tangra/ui/api': { singleton: true, requiredVersion: '^4.0.0', strictVersion: true, ...hostOnly },
}

export const remoteConfig = {
  name: 'sms_gw',
  filename: 'remoteEntry.js',
  manifest: true,
  exposes: { './routes': './src/routes.ts', './nav': './src/nav.ts' },
  shared,
  dts: false,
}
