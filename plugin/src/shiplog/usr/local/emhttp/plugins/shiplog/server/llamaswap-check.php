<?php
/* ShipLog — live llama-swap check for the settings page. Tests the URL + model
 * the user just typed (before Apply), straight from PHP so there's no CORS and
 * no dependency on the engine. Works with any OpenAI-compatible server that
 * exposes /v1/models. Returns {"ok":bool,"message":string}. */

header('Content-Type: application/json');

$url   = isset($_GET['url'])   ? trim($_GET['url'])   : '';
$model = isset($_GET['model']) ? trim($_GET['model']) : '';

if ($url === '' || $model === '') {
    echo json_encode(['ok' => false, 'message' => 'Enter the llama-swap URL and model first.']);
    exit;
}
if (!preg_match('#^https?://#i', $url)) {
    echo json_encode(['ok' => false, 'message' => 'URL must start with http:// or https://']);
    exit;
}
$url = rtrim($url, '/');

$ch = curl_init($url . '/v1/models');
curl_setopt_array($ch, [
    CURLOPT_RETURNTRANSFER => true,
    CURLOPT_CONNECTTIMEOUT => 3,
    CURLOPT_TIMEOUT        => 8,
]);
$body = curl_exec($ch);
$code = (int) curl_getinfo($ch, CURLINFO_HTTP_CODE);
curl_close($ch);

if ($body === false || $code === 0) {
    echo json_encode(['ok' => false, 'message' => 'Cannot reach llama-swap at ' . $url]);
    exit;
}
if ($code !== 200) {
    echo json_encode(['ok' => false, 'message' => 'llama-swap returned HTTP ' . $code]);
    exit;
}

$data   = json_decode($body, true);
$models = (is_array($data) && isset($data['data'])) ? $data['data'] : [];
$names  = [];
foreach ($models as $m) {
    if (isset($m['id'])) {
        $names[] = $m['id'];
    }
}
$found = in_array($model, $names, true);

if ($found) {
    echo json_encode(['ok' => true, 'message' => 'Reachable, model "' . $model . '" found.']);
} else {
    $avail = $names ? implode(', ', array_slice($names, 0, 8)) : '(none)';
    echo json_encode(['ok' => false, 'message' => 'Reachable, but model "' . $model . '" not found. Available: ' . $avail]);
}
