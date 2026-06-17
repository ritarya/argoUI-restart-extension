import { RFCValidationTab } from './RFCValidationTab';

((win: Window & typeof globalThis & {
  extensionsAPI?: Record<string, unknown>;
}) => {
  const api = win.extensionsAPI;
  console.log('[rfc-extension] extensionsAPI:', api);
  console.log('[rfc-extension] available methods:', api ? Object.keys(api) : 'API not available');

  if (!api) {
    console.error('[rfc-extension] window.extensionsAPI is not defined — extension will not load');
    return;
  }

  const register = api['registerResourceExtension'] as Function | undefined;
  if (typeof register !== 'function') {
    console.error('[rfc-extension] registerResourceExtension not found. Available:', Object.keys(api));
    return;
  }

  register(
    RFCValidationTab,
    'apps',
    'Deployment',
    'RFC Validation',
    { icon: 'fa-shield-check' }
  );
  console.log('[rfc-extension] registered RFCValidationTab for apps/Deployment');
})(window);
