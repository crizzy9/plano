Pod::Spec.new do |s|
  s.name = 'PlanoLive'
  s.version = '0.1.0'
  s.summary = 'Plano ActivityKit integration'
  s.description = 'Creates and registers native live flight activities.'
  s.license = { :type => 'MIT' }
  s.author = 'Plano'
  s.homepage = 'https://github.com/crizzy9/plano'
  s.platforms = { :ios => '16.4' }
  s.swift_version = '5.9'
  s.source = { :git => 'https://github.com/crizzy9/plano.git' }
  s.source_files = '*.swift'
  s.static_framework = true
  s.dependency 'ExpoModulesCore'
  s.frameworks = 'ActivityKit', 'Security'
  s.pod_target_xcconfig = { 'DEFINES_MODULE' => 'YES' }
end
