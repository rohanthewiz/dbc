// Monaco's editor worker, bootstrapped from this server. Monaco asks
// MonacoEnvironment.getWorkerUrl for a script to start its worker with (see
// editor.js); this one tells the worker where the vendored copy lives and
// loads it. Being a same-origin file, it needs nothing more than the page's
// CSP already allows — a data: URI shim would need worker-src data:.
self.MonacoEnvironment = { baseUrl: self.location.origin + "/static/vendor/monaco/" };
importScripts(self.location.origin + "/static/vendor/monaco/vs/base/worker/workerMain.js");
