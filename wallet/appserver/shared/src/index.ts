// Barrel for the shared core. Consumers may import named members from here, or
// import a specific module directly via the @bossw/shared/<module> subpath
// (which is what newui does to keep the original import shape).

export * from './application'
export * from './assets'
export * from './hooks'
export * from './http'
export * from './intl'
export * from './registry'

export { default as app } from './application'
export { default as Doc } from './doc'
export { default as t } from './intl'
export { default as State } from './state'
export { default as ws } from './ws'
