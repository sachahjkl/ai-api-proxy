{
  pkgs,
  module,
  package,
  dockerImage,
}:
pkgs.testers.runNixOSTest {
  name = "codex-proxy-deployment";
  nodes.machine = {...}: {
    imports = [module];
    virtualisation = {
      memorySize = 2048;
      diskSize = 4096;
      docker.enable = true;
    };
    services.codex-proxy = {
      enable = true;
      inherit package;
      publicUrl = "https://proxy.test";
      oauthCredentialFile = "/run/oauth-seed.json";
      proxyTokenFile = "/run/proxy-token";
    };
    environment.systemPackages = [pkgs.curl];
  };
  testScript = ''
    import json

    start_all()
    machine.wait_for_unit("multi-user.target")
    # These credentials are test fixtures, not usable OAuth tokens.
    seed = json.dumps({"access": "test-access", "refresh": "test-refresh", "expires": 4102444800000, "account_id": "test-account"})
    machine.succeed("printf '%s' '" + seed + "' > /run/oauth-seed.json")
    machine.succeed("printf '%s' 'test-secret' > /run/proxy-token")
    machine.succeed("chmod 600 /run/oauth-seed.json /run/proxy-token")
    machine.succeed("systemctl reset-failed codex-proxy; systemctl restart codex-proxy")
    machine.wait_for_unit("codex-proxy.service")
    machine.wait_for_open_port(8080)
    machine.succeed("curl --fail http://127.0.0.1:8080/healthz")
    machine.succeed("curl --fail -H 'Proxy-Authorization: Bearer test-secret' http://127.0.0.1:8080/readyz")
    machine.succeed("test $(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:8080/readyz) = 401")
    machine.succeed("test $(stat -Lc %a /var/lib/codex-proxy) = 700")
    machine.succeed("systemctl restart codex-proxy")
    machine.wait_for_open_port(8080)
    machine.succeed("curl --fail -H 'Proxy-Authorization: Bearer test-secret' http://127.0.0.1:8080/readyz")

    machine.wait_for_unit("docker.service")
    machine.succeed("docker load < ${dockerImage}")
    machine.succeed("chown 65532:65532 /run/oauth-seed.json; chmod 400 /run/oauth-seed.json")
    machine.succeed("docker volume create proxy-state")
    machine.succeed("docker run -d --name proxy --read-only --cap-drop ALL --security-opt no-new-privileges -p 127.0.0.1:8081:8080 -e PUBLIC_URL=https://proxy.test -e PROXY_TOKEN=test-secret -e OAUTH_CREDENTIAL_FILE=/run/oauth.json -e OAUTH_STATE_FILE=/var/lib/codex-proxy/oauth.json -v /run/oauth-seed.json:/run/oauth.json:ro -v proxy-state:/var/lib/codex-proxy codex-proxy:0.1.0")
    machine.wait_until_succeeds("curl --fail http://127.0.0.1:8081/healthz")
    machine.succeed("curl --fail -H 'Proxy-Authorization: Bearer test-secret' http://127.0.0.1:8081/readyz")
    machine.succeed("test $(stat -c %u /var/lib/docker/volumes/proxy-state/_data) = 65532")
    machine.succeed("test $(stat -c %a /var/lib/docker/volumes/proxy-state/_data) = 700")
    machine.succeed("docker stop --time 15 proxy")
    machine.succeed("test $(docker inspect -f '{{.State.ExitCode}}' proxy) = 0")
    machine.succeed("systemctl stop codex-proxy")
    machine.succeed("test $(systemctl show -p ExecMainStatus --value codex-proxy) = 0")
  '';
}
