const { withAppBuildGradle } = require('expo/config-plugins');

const marker = '// @generated begin Plano Android release signing';
const releaseSigning = String.raw`            ${marker}
            def planoPreviewFlag = providers.environmentVariable('PLANO_ANDROID_PREVIEW_BUILD').orNull
            if (planoPreviewFlag != null && planoPreviewFlag != '1') {
                throw new GradleException('PLANO_ANDROID_PREVIEW_BUILD must be 1 or unset.')
            }
            def planoSigningInputs = [
                'PLANO_ANDROID_KEYSTORE_FILE',
                'PLANO_ANDROID_KEY_ALIAS',
                'PLANO_ANDROID_STORE_PASSWORD_FILE',
                'PLANO_ANDROID_KEY_PASSWORD_FILE',
            ].collectEntries { name -> [(name): providers.environmentVariable(name).orNull] }
            if (planoSigningInputs.values().any { it != null }) {
                if (planoPreviewFlag != null || planoSigningInputs.values().any { it == null || it.isBlank() }) {
                    throw new GradleException('Set all four Plano Android signing variables and unset PLANO_ANDROID_PREVIEW_BUILD.')
                }
                def signingFile = { name ->
                    def file = new File(planoSigningInputs[name])
                    if (!file.isAbsolute() || !file.isFile() || !file.canRead()) {
                        throw new GradleException(name + ' must name a readable absolute file.')
                    }
                    file
                }
                def signingPassword = { name ->
                    try {
                        return signingFile(name).getText('UTF-8').replaceFirst(/(?:\r\n|\n|\r)\z/, '')
                    } catch (IOException ignored) {
                        throw new GradleException('Unable to read ' + name + '.')
                    }
                }
                signingConfig = signingConfigs.create('planoRelease') {
                    storeFile signingFile('PLANO_ANDROID_KEYSTORE_FILE')
                    keyAlias planoSigningInputs['PLANO_ANDROID_KEY_ALIAS']
                    storePassword signingPassword('PLANO_ANDROID_STORE_PASSWORD_FILE')
                    keyPassword signingPassword('PLANO_ANDROID_KEY_PASSWORD_FILE')
                }
            } else {
                signingConfig = planoPreviewFlag == '1' ? signingConfigs.debug : null
            }
            // @generated end Plano Android release signing`;

module.exports = (config) => withAppBuildGradle(config, (current) => {
  const gradle = current.modResults;
  if (gradle.language !== 'groovy') {
    throw new Error('Plano Android release signing requires the Expo Groovy Gradle template.');
  }

  const buildTypesBlocks = gradle.contents.match(/^    buildTypes \{[\s\S]*?^    \}(?=\r?$)/gm);
  const buildTypes = buildTypesBlocks?.length === 1 ? buildTypesBlocks[0] : undefined;
  const releaseBlocks = buildTypes?.match(/^        release \{[\s\S]*?^        \}(?=\r?$)/gm);
  const release = releaseBlocks?.length === 1 ? releaseBlocks[0] : undefined;
  const signing = /^            signingConfig signingConfigs\.debug(?=\r?$)/gm;
  const defaults = release?.match(signing)?.length ?? 0;
  const generated = gradle.contents.includes('\r\n') ? releaseSigning.replaceAll('\n', '\r\n') : releaseSigning;
  if (release?.includes(generated) && defaults === 0) return current;
  if (!release || defaults !== 1 || gradle.contents.includes(marker)) {
    throw new Error('Plano could not locate the expected release signing block in android/app/build.gradle.');
  }

  const updatedRelease = release.replace(signing, () => generated);
  const updatedBuildTypes = buildTypes.replace(release, () => updatedRelease);
  gradle.contents = gradle.contents.replace(buildTypes, () => updatedBuildTypes);
  return current;
});
